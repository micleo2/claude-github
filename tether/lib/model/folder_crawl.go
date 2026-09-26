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
	"slices"
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
	crawlWindowInit = 16
	crawlWindowMax  = 1024
	// Bytes downloaded ahead and not yet opened, at most; a tenth of the
	// cache budget if that is smaller.
	crawlPendingMax = 256 << 20
	// A prefetched file this many files behind the walker's position was
	// passed by. Small, since the order is known (sorted walkers are
	// recognised), so that files a filtering walker skips are learned
	// quickly.
	crawlSkipSlack = 8
	// Without an order (orderSweep), the whole subtree is fetched, as
	// EdenFS's walk detector does, but paused while fewer than 1 in
	// crawlSweepMinUse of the files fetched this far were opened, once
	// crawlSweepLag of them were not.
	crawlSweepMinUse = 10
	crawlSweepLag    = 1000
	crawlMaxActors   = 64
	crawlMaxProduced = 1 << 16
	// A misprediction is explained by an order if the missed file is
	// among this many files after the walker's last position in it.
	crawlExplainAhead = 32
	// Mispredictions neither order explains before we stop following the
	// walker's position and sweep its subtree instead.
	crawlUnexplainedMax   = 3
	crawlUnexplainedShare = 8 // ... and at least 1 in this many of its opens
	// A file extension or directory name is excluded from predictions
	// once the walker passed this many predicted files with it and opened
	// none.
	crawlExcludeAfter = 8
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
	// Placeholder opens waiting for their download. A process that waits
	// for one open at a time never has two: two means several threads or
	// processes walk at once.
	inflight int
	parallel bool
	lane     *crawlLane // once detected
}

type crawlTouch struct {
	at  time.Time
	dir string
}

// crawlOrder is how we expect a walker to visit the entries of a
// directory.
type crawlOrder int

const (
	orderDirectory crawlOrder = iota // as listed (getdents): find, grep -r
	orderSorted                      // by name: rg --sort path, git (index order)
	orderSweep                       // no usable order (parallel walkers, rg): cover the subtree
)

func (o crawlOrder) String() string {
	return [...]string{"directory", "sorted", "sweep"}[o]
}

// crawlLane is the prediction for one walker. Fields other than walker are
// protected by crawlTracker.mut; walker belongs to the refill goroutine
// (refilling).
type crawlLane struct {
	exe         string
	root        string // the walk is below this directory
	restart     string // reposition the walk after this file
	refilling   bool
	walker      *treeWalker
	walkRoot    string
	done        bool // walk finished
	order       crawlOrder
	last        string   // the walker's last known position
	misses      []string // unpredicted placeholders it opened, to be explained
	unexplained int

	// What the walker passes by without opening: by file extension and by
	// the name of any directory the file is in.
	skipped, used map[string]int
	purge         bool // an exclusion was learned
	rewalk        bool // start the sweep over from the root

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
// already (hit: we prefetched it) or a placeholder it now waits for. The
// caller calls the returned function once the open has been served.
func (f *sendReceiveFolder) crawlTouch(pid int, name string, hit bool) (served func()) {
	served = func() {}
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
	if !hit {
		a.inflight++
		if a.inflight > 1 && !a.parallel {
			a.parallel = true
			if a.lane != nil && a.lane.order != orderSweep {
				a.lane.order = orderSweep
				a.lane.rewalk = true
				clear(a.lane.skipped)
			}
		}
		served = func() {
			t.mut.Lock()
			a.inflight--
			t.mut.Unlock()
		}
	}

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
			last:     name,
			ahead:    make(map[string]aheadFile),
			produced: make(map[string]struct{}),
			skipped:  make(map[string]int),
			used:     make(map[string]int),
			window:   crawlWindowInit,
		}
		if a.parallel {
			l.order = orderSweep
			l.rewalk = true
		}
		a.lane = l
		a.touches = nil
		f.sl.Info("Prefetching ahead of a tree walk", "exe", exe, "pgid", id, slogutil.FilePath(root))
	} else if af, ok := l.ahead[name]; ok {
		delete(l.ahead, name)
		l.pending -= af.size
		l.opened++
		l.note(l.used, name)
		if af.seq > l.pos {
			l.pos = af.seq
			l.last = name
		}
		if !hit {
			// The walker waits for a file we are still fetching: go further
			// ahead.
			l.waited++
			l.window = min(l.window*2, crawlWindowMax)
		}
		// Without an order, nothing tells us what the walker passed by.
		for n, o := range l.ahead {
			if l.order != orderSweep && o.seq < l.pos-crawlSkipSlack {
				delete(l.ahead, n)
				l.pending -= o.size
				l.unused = append(l.unused, n)
				l.window = max(l.window-1, crawlWindowMin)
				for _, k := range l.traits(n) {
					l.skipped[k]++
					if l.skipped[k] == crawlExcludeAfter && l.used[k] == 0 {
						l.purge = true // drop what is queued like it
					}
				}
			}
		}
	} else if !hit {
		// A placeholder we did not predict: the walk went elsewhere.
		l.missed++
		l.note(l.used, name)
		if !inDir(name, l.root) {
			l.root = commonDir(l.root, dir)
		}
		if _, ok := l.produced[name]; !ok && len(l.misses) < 16 {
			l.misses = append(l.misses, name)
		}
	}
	if !l.refilling {
		l.refilling = true
		go f.crawlRefill(a, l)
	}
	return served
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
		misses, last, order := l.misses, l.last, l.order
		l.misses = nil
		sweepFromStart := l.rewalk
		l.rewalk = false
		if l.purge {
			l.purge = false
			dropped := f.od.pf.filterLane(a.id, func(name string) bool { return !l.excluded(name) })
			for _, name := range dropped {
				if af, ok := l.ahead[name]; ok {
					delete(l.ahead, name)
					l.pending -= af.size
				}
			}
			f.sl.Debug("Tree walk skips some files; not prefetching files like them", "exe", l.exe, "pgid", a.id, "dropped", len(dropped), "opened", l.opened, "prefetched", len(l.produced))
		}
		if restart != "" || sweepFromStart {
			l.done = false
		}
		need := l.window - len(l.ahead)
		if l.order == orderSweep {
			need = crawlWindowMax - len(l.ahead)
			if n := len(l.produced); n-l.opened > crawlSweepLag && l.opened*crawlSweepMinUse < n {
				need = 0
			}
		}
		pendingRoom, totalRoom := pendingMax-l.pending, maxTotal-l.total // bytes
		if maxFile <= 0 || pendingRoom <= 0 || totalRoom <= 0 || l.capped {
			need = 0
		}
		if (need <= 0 || l.done) && restart == "" && len(unused) == 0 && len(misses) == 0 && l.walkRoot == root && !sweepFromStart {
			l.refilling = false
			t.mut.Unlock()
			return
		}
		t.mut.Unlock()

		for _, name := range unused {
			f.crawlUnmark(name)
		}
		// Which order explains where the walker went?
		base := f.mtimefs.URI()
		unexplained := 0
		for _, m := range misses {
			if order == orderSweep {
				break
			}
			switch {
			case explains(base, root, last, m, order):
			case explains(base, root, last, m, 1-order):
				order = 1 - order
				f.sl.Debug("Tree walk follows another order", "exe", l.exe, "pgid", a.id, "order", order)
			default:
				unexplained++
				f.sl.Debug("Tree walk went somewhere we did not expect", "exe", l.exe, "pgid", a.id, slogutil.FilePath(m), "after", last, "order", order)
			}
			restart, last = m, m
		}
		if len(misses) > 0 {
			t.mut.Lock()
			l.unexplained += unexplained
			// Only if they are a real share of its opens: a sequential
			// walker that reads a few other files (git reading objects
			// after comparing the work tree) keeps its order.
			if l.order != orderSweep && l.unexplained >= crawlUnexplainedMax && l.unexplained*crawlUnexplainedShare >= l.opened {
				order = orderSweep
				f.sl.Debug("Tree walk has no order we can follow; covering its subtree", "exe", l.exe, "pgid", a.id)
			}
			l.last = last
			if order == orderSweep {
				// Parallel walkers are ahead and behind their latest
				// position at once: cover the whole subtree.
				sweepFromStart = sweepFromStart || l.order != orderSweep || restart != ""
				restart = ""
				l.done = false
			}
			if order != l.order {
				// What the walker seemed to pass by in the wrong order
				// means nothing.
				clear(l.skipped)
			}
			l.order = order
			t.mut.Unlock()
		}
		if order == orderSweep && (sweepFromStart || l.walker == nil || l.walkRoot != root) {
			l.walker = newTreeWalker(base, root, "", false)
			l.walkRoot = root
		} else if restart != "" || l.walker == nil || l.walkRoot != root || l.walker.sorted != (order == orderSorted) {
			if restart == "" {
				restart = l.walker.position()
			}
			l.walker = newTreeWalker(base, root, restart, order == orderSorted)
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
			t.mut.Lock()
			excluded := l.order != orderSweep && l.excluded(name)
			t.mut.Unlock()
			if excluded {
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
			// A sweep says nothing about where the walker goes next; it
			// widens only when the walker opens a file outside it.
			if l.order != orderSweep && l.root != "" && l.root == l.walkRoot && l.restart == "" {
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

// note counts name's extension and the names of the directories it is in
// in stats.
func (l *crawlLane) note(stats map[string]int, name string) {
	for _, k := range l.traits(name) {
		stats[k]++
	}
}

func (l *crawlLane) traits(name string) []string {
	parts := strings.Split(name, "/")
	keys := make([]string, 0, len(parts))
	keys = append(keys, "ext:"+path.Ext(parts[len(parts)-1]))
	for _, d := range parts[:len(parts)-1] {
		keys = append(keys, "dir:"+d)
	}
	return keys
}

// excluded reports whether the walker keeps passing by files like name.
func (l *crawlLane) excluded(name string) bool {
	for _, k := range l.traits(name) {
		if l.skipped[k] >= crawlExcludeAfter && l.used[k] == 0 {
			return true
		}
	}
	return false
}

// explains reports whether a walk in order o from the file after
// continues to name within crawlExplainAhead files.
func explains(base, root, after, name string, o crawlOrder) bool {
	if after == "" || o == orderSweep {
		return false
	}
	w := newTreeWalker(base, root, after, o == orderSorted)
	for range crawlExplainAhead {
		n, ok := w.next()
		if !ok {
			return false
		}
		if n == name {
			return true
		}
	}
	return false
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
	base   string // the folder's absolute path
	sorted bool   // by name instead of directory order
	stack  []walkFrame
}

type walkFrame struct {
	dir     string
	entries []os.DirEntry
	i       int
}

// newTreeWalker walks root, starting after the file after if it is below
// root.
func newTreeWalker(base, root, after string, sorted bool) *treeWalker {
	w := &treeWalker{base: base, sorted: sorted}
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
		if w.sorted {
			slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		}
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
