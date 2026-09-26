// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// Sibling prefetch for on-demand folders.
//
// Tools that walk a tree (grep -r, builds, IDE indexers) open files one at a
// time, so each placeholder costs a full fetch round trip while the tool
// waits. When an application opens a placeholder, we queue the small
// placeholders in the same directory; a pool of workers downloads them in
// parallel with the file the application is waiting for, so the next files
// it opens are usually already local. See docs/research/small-file-hydration.md.

package model

import (
	"context"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/protocol"
)

const (
	// Queue at most this many siblings per trigger, and at most this many
	// in total.
	prefetchPerDirectory = 512
	prefetchQueueLimit   = 8192
	// Stop scanning a directory's index entries after this many; very large
	// directories get a partial prefetch rather than a slow scan.
	prefetchScanLimit = 20000
	// A directory is considered at most once per this interval.
	prefetchDirInterval = 30 * time.Second
)

// prefetchQueue holds speculative downloads. Siblings of opened files come
// first: they are what the application needs next. Then one lane per tree
// walker (folder_crawl.go), served round-robin.
type prefetchQueue struct {
	mut     sync.Mutex
	queue   []string
	queued  map[string]struct{}
	dirSeen map[string]time.Time
	lanes   map[int][]string // by crawl actor
	order   []int            // lanes, round-robin
	next    int
	wake    chan struct{}
}

func newPrefetchQueue() prefetchQueue {
	return prefetchQueue{
		queued:  make(map[string]struct{}),
		dirSeen: make(map[string]time.Time),
		lanes:   make(map[int][]string),
		wake:    make(chan struct{}, 1),
	}
}

// claimDir reports whether dir should be scanned now, and records that it
// was.
func (q *prefetchQueue) claimDir(dir string) bool {
	q.mut.Lock()
	defer q.mut.Unlock()
	now := time.Now()
	if t, ok := q.dirSeen[dir]; ok && now.Sub(t) < prefetchDirInterval {
		return false
	}
	q.dirSeen[dir] = now
	if len(q.dirSeen) > 4096 {
		for d, t := range q.dirSeen {
			if now.Sub(t) >= prefetchDirInterval {
				delete(q.dirSeen, d)
			}
		}
	}
	return true
}

func (q *prefetchQueue) push(names []string) int {
	return q.pushLane(0, names)
}

// pushLane queues names in the lane of a crawl actor, or with the siblings
// for lane 0. Names already queued anywhere are skipped.
func (q *prefetchQueue) pushLane(lane int, names []string) int {
	q.mut.Lock()
	n := 0
	for _, name := range names {
		if len(q.queued) >= prefetchQueueLimit {
			break
		}
		if _, ok := q.queued[name]; ok {
			continue
		}
		q.queued[name] = struct{}{}
		if lane == 0 {
			q.queue = append(q.queue, name)
		} else {
			if _, ok := q.lanes[lane]; !ok {
				q.order = append(q.order, lane)
			}
			q.lanes[lane] = append(q.lanes[lane], name)
		}
		n++
	}
	q.mut.Unlock()
	if n > 0 {
		// Wake one idle worker; workers wake each other while there is work.
		select {
		case q.wake <- struct{}{}:
		default:
		}
	}
	return n
}

// dropLane forgets the queued files of a crawl actor.
func (q *prefetchQueue) dropLane(lane int) {
	q.mut.Lock()
	defer q.mut.Unlock()
	for _, name := range q.lanes[lane] {
		delete(q.queued, name)
	}
	delete(q.lanes, lane)
	if i := slices.Index(q.order, lane); i >= 0 {
		q.order = slices.Delete(q.order, i, i+1)
	}
}

// pop returns the next file to prefetch and the lane it came from.
func (q *prefetchQueue) pop() (string, int, bool) {
	q.mut.Lock()
	defer q.mut.Unlock()
	if len(q.queue) > 0 {
		name := q.queue[0]
		q.queue = q.queue[1:]
		delete(q.queued, name)
		return name, 0, true
	}
	for range len(q.order) {
		q.next %= len(q.order)
		lane := q.order[q.next]
		q.next++
		if names := q.lanes[lane]; len(names) > 0 {
			q.lanes[lane] = names[1:]
			delete(q.queued, names[0])
			return names[0], lane, true
		}
	}
	return "", 0, false
}

func (q *prefetchQueue) len() int {
	q.mut.Lock()
	defer q.mut.Unlock()
	n := len(q.queue)
	for _, names := range q.lanes {
		n += len(names)
	}
	return n
}

// prefetchSiblings queues the small placeholders next to name. It returns
// immediately; the directory is scanned in the background.
func (f *sendReceiveFolder) prefetchSiblings(name string) {
	maxSize := int64(f.liveConfig().PrefetchMaxFileKiB) * 1024
	if maxSize <= 0 {
		return
	}
	dir := path.Dir(name)
	if dir == "." {
		dir = ""
	}
	if !f.od.pf.claimDir(dir) {
		return
	}
	go func() {
		prefix := dir
		if prefix != "" {
			prefix += "/"
		}
		var names []string
		scanned := 0
		it, errFn := f.model.sdb.AllLocalFilesWithPrefix(f.folderID, protocol.LocalDeviceID, dir)
		for fi := range it {
			scanned++
			if scanned > prefetchScanLimit || len(names) >= prefetchPerDirectory {
				break
			}
			rest, ok := strings.CutPrefix(fi.Name, prefix)
			if !ok || rest == "" || strings.Contains(rest, "/") {
				continue // not a direct child
			}
			if fi.Name == name || !fi.IsVirtual() || fi.IsDeleted() || fi.Type != protocol.FileInfoTypeFile || fi.Size > maxSize {
				continue
			}
			names = append(names, fi.Name)
		}
		if err := errFn(); err != nil {
			f.sl.Debug("Prefetch scan failed", slogutil.FilePath(dir), slogutil.Error(err))
			return
		}
		if n := f.od.pf.push(names); n > 0 {
			f.sl.Debug("Prefetching siblings", slogutil.FilePath(dir), "files", n)
		}
	}()
}

func (f *sendReceiveFolder) prefetchWorker(ctx context.Context) {
	for {
		name, lane, ok := f.od.pf.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-f.od.pf.wake:
				continue
			}
		}
		if ctx.Err() != nil {
			return
		}
		if f.od.pf.len() > 0 {
			// Chain-wake another idle worker so the whole pool spins up.
			select {
			case f.od.pf.wake <- struct{}{}:
			default:
			}
		}
		kind := hydratePrefetch
		if lane != 0 {
			kind = hydrateLookahead
		}
		if err := f.hydrateName(ctx, name, kind); err != nil {
			// Prefetch is best effort; the file is fetched on open.
			f.sl.Debug("Prefetch failed", slogutil.FilePath(name), slogutil.Error(err))
		}
	}
}
