// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Prefetch ahead of tree walks ("crawlers") in on-demand folders.
//
// Sibling prefetch (folder_prefetch.go) helps once a tool reaches a
// directory, but a tool walking the tree (grep -r, find -exec, a build)
// still waits a full fetch for the first placeholder it opens in every
// directory. When a process group opens placeholders in several
// directories in quick succession, we treat it as walking the tree: we
// list the tree below the directories it touched in the order the kernel
// returns entries (the order find and grep -r visit them), and keep a
// window of placeholders downloading ahead of it.
//
// We learn how far the walker has got because files prefetched for it stay
// marked until they are first opened (hsmHandler.OpenedMarked); opening a
// placeholder we did not predict moves the prediction there. The window
// grows when the walker catches up with the downloads, and shrinks when it
// passes files by without opening them. See docs/design/crawler-prefetch.md.

package model

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/hsm"
)

const (
	// A process group becomes a walker when it opens placeholders in this
	// many directories within crawlDetectWindow. Only placeholders count:
	// find, du and ls -R never open file content, and never qualify.
	crawlDetectDirs   = 4
	crawlDetectWindow = 2 * time.Second
	// Walkers are forgotten once their process group is gone, or after
	// this long without opening anything we prefetched or predicted.
	crawlIdle = 30 * time.Second
	// Files downloaded ahead of the walker and not yet opened by it.
	crawlWindowMin  = 16
	crawlWindowInit = 64
	crawlWindowMax  = 1024
	// Bytes downloaded ahead and not yet opened, at most; a tenth of the
	// cache budget if that is smaller.
	crawlPendingMax = 256 << 20
	// A prefetched file this many files behind the walker's position was
	// passed by (a tool that sorts entries itself reorders within a
	// directory, so not immediately).
	crawlSkipSlack   = 64
	crawlMaxActors   = 64
	crawlMaxProduced = 1 << 16
	// Entries listed per refill round, so that a subtree of local files
	// does not keep a refill going unchecked.
	crawlScanChunk = 4096
)

type crawlTracker struct {
	mut    sync.Mutex
	actors map[int]*crawlActor // by process group
}

type crawlActor struct {
	id       int
	touches  []crawlTouch // while detecting
	lastSeen time.Time
	lane     *crawlLane // once detected
}

type crawlTouch struct {
	at  time.Time
	dir string
}

// crawlLane is the prediction for one walker. Fields other than walker are
// protected by crawlTracker.mut; walker belongs to the refill goroutine
// (refilling).
type crawlLane struct {
	exe       string
	root      string // the walk is below this directory
	restart   string // reposition the walk after this file
	refilling bool
	walker    *treeWalker
	walkRoot  string
	done      bool // walk finished

	ahead    map[string]aheadFile // prefetched or queued, not yet opened
	produced map[string]struct{}
	unused   []string // passed by; to be unmarked
	seq, pos int
	window   int
	pending  int64 // bytes in ahead
	total    int64 // bytes handed to the prefetcher in all
	capped   bool
	opened   int // prefetched files the walker opened
	waited   int // ... while we were still fetching them
	missed   int // placeholders it opened that we had not predicted
}

type aheadFile struct {
	seq  int
	size int64
}

// crawlTouch records that process pid opened name, which was complete
// already (hit: we prefetched it) or a placeholder it now waits for.
func (f *sendReceiveFolder) crawlTouch(pid int, name string, hit bool) {
	// A process we cannot see (another PID namespace) is reported as 0.
	if pid <= 0 || f.liveConfig().CrawlPrefetchMaxFileKiB <= 0 {
		return
	}
	id := processGroup(pid)
	dir := parentDir(name)
	now := time.Now()
	t := &f.od.crawl

	t.mut.Lock()
	defer t.mut.Unlock()
	if t.actors == nil {
		t.actors = make(map[int]*crawlActor)
	}
	a := t.actors[id]
	if a == nil {
		if hit || len(t.actors) >= crawlMaxActors {
			return
		}
		a = &crawlActor{id: id}
		t.actors[id] = a
	}
	a.lastSeen = now

	l := a.lane
	if l == nil {
		if hit {
			return
		}
		keep := a.touches[:0]
		for _, tc := range a.touches {
			if now.Sub(tc.at) < crawlDetectWindow {
				keep = append(keep, tc)
			}
		}
		a.touches = append(keep, crawlTouch{now, dir})
		dirs := make(map[string]struct{})
		root := dir
		for _, tc := range a.touches {
			dirs[tc.dir] = struct{}{}
			root = commonDir(root, tc.dir)
		}
		if len(dirs) < crawlDetectDirs {
			return
		}
		exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		l = &crawlLane{
			exe:      exe,
			root:     root,
			restart:  name,
			ahead:    make(map[string]aheadFile),
			produced: make(map[string]struct{}),
			window:   crawlWindowInit,
		}
		a.lane = l
		a.touches = nil
		f.sl.Info("Prefetching ahead of a tree walk", "exe", exe, "pgid", id, slogutil.FilePath(root))
	} else if af, ok := l.ahead[name]; ok {
		delete(l.ahead, name)
		l.pending -= af.size
		l.opened++
		l.pos = max(l.pos, af.seq)
		if !hit {
			// The walker waits for a file we are still fetching: go further
			// ahead.
			l.waited++
			l.window = min(l.window*2, crawlWindowMax)
		}
		for n, o := range l.ahead {
			if o.seq < l.pos-crawlSkipSlack {
				delete(l.ahead, n)
				l.pending -= o.size
				l.unused = append(l.unused, n)
				l.window = max(l.window-1, crawlWindowMin)
			}
		}
	} else if !hit {
		// A placeholder we did not predict: the walk went elsewhere.
		l.missed++
		if !inDir(name, l.root) {
			l.root = commonDir(l.root, dir)
			l.restart = name
		} else if _, ok := l.produced[name]; !ok {
			l.restart = name
		}
	}
	if !l.refilling {
		l.refilling = true
		go f.crawlRefill(a, l)
	}
}

// crawlRefill tops up the lane's window from its walk.
func (f *sendReceiveFolder) crawlRefill(a *crawlActor, l *crawlLane) {
	t := &f.od.crawl
	for {
		cfg := f.liveConfig()
		maxFile := int64(cfg.CrawlPrefetchMaxFileKiB) * 1024
		maxTotal := int64(cfg.CrawlPrefetchMaxMiB) << 20
		pendingMax := int64(crawlPendingMax)
		if b := cfg.CacheBudget; b.Value > 0 && !b.Percentage() {
			pendingMax = min(pendingMax, int64(b.BaseValue())/10)
		}

		t.mut.Lock()
		if a.lane != l {
			t.mut.Unlock()
			return
		}
		restart, root := l.restart, l.root
		l.restart = ""
		unused := l.unused
		l.unused = nil
		if restart != "" {
			l.done = false
		}
		need := l.window - len(l.ahead)
		pendingRoom, totalRoom := pendingMax-l.pending, maxTotal-l.total // bytes
		if maxFile <= 0 || pendingRoom <= 0 || totalRoom <= 0 || l.capped {
			need = 0
		}
		if (need <= 0 || l.done) && restart == "" && len(unused) == 0 {
			l.refilling = false
			t.mut.Unlock()
			return
		}
		t.mut.Unlock()

		for _, name := range unused {
			f.crawlUnmark(name)
		}
		if restart != "" || l.walker == nil || l.walkRoot != root {
			if restart == "" {
				restart = l.walker.position()
			}
			l.walker = newTreeWalker(f.mtimefs.URI(), root, restart)
			l.walkRoot = root
		}
		if need <= 0 {
			continue
		}

		var names []string
		var sizes []int64
		done, full, capped := false, false, false
		for scanned := 0; len(names) < need && scanned < crawlScanChunk; scanned++ {
			name, ok := l.walker.next()
			if !ok {
				done = true
				break
			}
			if _, ok := l.produced[name]; ok { // read without the lock: only we write it
				continue
			}
			abs := f.absPath(name)
			fi, err := os.Lstat(abs)
			if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxFile {
				continue
			}
			if _, _, ok := hsm.ReadPlaceholder(abs); !ok {
				continue
			}
			if fi.Size() > totalRoom {
				capped = true
				break
			}
			if fi.Size() > pendingRoom {
				// Keep it for when the walker has opened some of the
				// files ahead.
				l.walker.unread()
				full = true
				break
			}
			pendingRoom -= fi.Size()
			totalRoom -= fi.Size()
			names = append(names, name)
			sizes = append(sizes, fi.Size())
		}

		t.mut.Lock()
		if a.lane != l {
			t.mut.Unlock()
			return
		}
		if len(l.produced) > crawlMaxProduced {
			// Only avoids queueing a file twice; files already downloaded
			// are no longer placeholders and are skipped anyway.
			clear(l.produced)
		}
		for i, name := range names {
			l.seq++
			l.ahead[name] = aheadFile{seq: l.seq, size: sizes[i]}
			l.produced[name] = struct{}{}
			l.pending += sizes[i]
			l.total += sizes[i]
		}
		if done {
			if l.root != "" && l.root == l.walkRoot && l.restart == "" {
				// The directory we took for the walk's root is done, but
				// the walker may well go on (we only saw where it began).
				// Continue after it in its parent, with a small window
				// until the walker shows it follows.
				l.restart = l.root
				l.root = parentDir(l.root)
				l.window = crawlWindowMin
			} else {
				l.done = true
			}
		}
		if capped && !l.capped {
			l.capped = true
			f.sl.Info("Stopped prefetching ahead of a tree walk: it reached crawlPrefetchMaxMiB", "exe", l.exe, "pgid", a.id, "MiB", cfg.CrawlPrefetchMaxMiB)
		}
		if (full || capped) && len(names) == 0 {
			// Wait for the walker to open some of what we fetched.
			l.refilling = false
			t.mut.Unlock()
			return
		}
		t.mut.Unlock()
		f.od.pf.pushLane(a.id, names)
	}
}

// crawlExpects reports whether a walker has yet to open name, which we
// prefetched for it.
func (f *sendReceiveFolder) crawlExpects(name string) bool {
	t := &f.od.crawl
	t.mut.Lock()
	defer t.mut.Unlock()
	for _, a := range t.actors {
		if a.lane != nil {
			if _, ok := a.lane.ahead[name]; ok {
				return true
			}
		}
	}
	return false
}

// crawlUnmark removes the mark we kept on a prefetched file that the walker
// did not open.
func (f *sendReceiveFolder) crawlUnmark(name string) {
	if key, ok := fileKey(f.absPath(name)); ok {
		f.model.hsmUnmarkUnused(f.folderID, name, key)
	}
}

// crawlJanitor forgets walkers whose processes are gone or that went idle,
// and drops what is still queued for them.
func (f *sendReceiveFolder) crawlJanitor(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		t := &f.od.crawl
		var dropped []*crawlActor
		t.mut.Lock()
		for id, a := range t.actors {
			idle := time.Since(a.lastSeen) > crawlIdle
			if a.lane == nil && time.Since(a.lastSeen) > crawlDetectWindow || idle || !processGroupAlive(id) {
				delete(t.actors, id)
				if a.lane != nil {
					dropped = append(dropped, a)
				}
			}
		}
		var unused []string
		for _, a := range dropped {
			l := a.lane
			a.lane = nil
			for name := range l.ahead {
				unused = append(unused, name)
			}
			unused = append(unused, l.unused...)
			f.sl.Debug("Tree walk ended", "exe", l.exe, "pgid", a.id, "opened", l.opened, "waited", l.waited, "missed", l.missed, "prefetched", len(l.produced), "bytes", l.total, "window", l.window)
		}
		t.mut.Unlock()
		for _, a := range dropped {
			f.od.pf.dropLane(a.id)
		}
		for _, name := range unused {
			f.crawlUnmark(name)
		}
	}
}

// treeWalker lists the regular files below a directory depth-first, in the
// order the filesystem returns directory entries.
type treeWalker struct {
	base  string // the folder's absolute path
	stack []walkFrame
}

type walkFrame struct {
	dir     string
	entries []os.DirEntry
	i       int
}

// newTreeWalker walks root, starting after the file after if it is below
// root.
func newTreeWalker(base, root, after string) *treeWalker {
	w := &treeWalker{base: base}
	w.push(root)
	if after == "" || !inDir(after, root) {
		return w
	}
	rel := after
	if root != "" {
		rel = after[len(root)+1:]
	}
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		top := &w.stack[len(w.stack)-1]
		j := -1
		for k, e := range top.entries {
			if e.Name() == p {
				j = k
				break
			}
		}
		if j < 0 {
			break // gone: start at the beginning of this directory
		}
		top.i = j + 1
		if i < len(parts)-1 {
			w.push(path.Join(top.dir, p))
		}
	}
	return w
}

func (w *treeWalker) push(dir string) {
	var entries []os.DirEntry
	if d, err := os.Open(filepath.Join(w.base, dir)); err == nil {
		entries, _ = d.ReadDir(-1) // directory order, unsorted
		d.Close()
	}
	w.stack = append(w.stack, walkFrame{dir: dir, entries: entries})
}

func (w *treeWalker) next() (string, bool) {
	for len(w.stack) > 0 {
		top := &w.stack[len(w.stack)-1]
		if top.i >= len(top.entries) {
			w.stack = w.stack[:len(w.stack)-1]
			continue
		}
		e := top.entries[top.i]
		top.i++
		name := path.Join(top.dir, e.Name())
		switch {
		case e.IsDir():
			if top.dir == "" && (e.Name() == ".stfolder" || e.Name() == ".stversions") {
				continue
			}
			w.push(name)
		case e.Type().IsRegular():
			return name, true
		}
	}
	return "", false
}

// unread steps back over the file next just returned.
func (w *treeWalker) unread() {
	w.stack[len(w.stack)-1].i--
}

// position returns the entry most recently passed, for restarting the walk
// under a different root.
func (w *treeWalker) position() string {
	for i := len(w.stack) - 1; i >= 0; i-- {
		if fr := w.stack[i]; fr.i > 0 {
			return path.Join(fr.dir, fr.entries[fr.i-1].Name())
		}
	}
	return ""
}

func parentDir(name string) string {
	if d := path.Dir(name); d != "." {
		return d
	}
	return ""
}

// inDir reports whether name is below dir ("" is the folder root).
func inDir(name, dir string) bool {
	return dir == "" || strings.HasPrefix(name, dir+"/")
}

// commonDir returns the deepest directory containing both a and b.
func commonDir(a, b string) string {
	for a != "" && a != b && !inDir(b, a) {
		a = parentDir(a)
	}
	return a
}
