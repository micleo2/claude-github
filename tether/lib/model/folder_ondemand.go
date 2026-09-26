// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

// On-demand ("online-only") files for send-receive folders.
//
// Files that are not pinned are written as placeholders: sparse files with
// the right size, mode and mtime, marked by xattrs (see lib/hsm). Their index
// entries carry the full block list and FlagLocalVirtual. When an
// application opens a placeholder through the folder's view, lib/hsm blocks
// it and calls Hydrate, which fetches the blocks from peers exactly like the
// puller does, verifies them and writes them in place.

package model

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/hsm"
	"github.com/syncthing/syncthing/lib/ignore"
	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/scanner"
)

const (
	onDemandMaintenanceInterval = time.Minute
	hydrationConcurrency        = 4
	// Right after start, connections to peers are still being set up. A
	// hydration without any source waits for one during this period
	// instead of failing at once.
	hydrationStartupGrace = 30 * time.Second
)

var (
	errNoPlaceholderSource = errors.New("the index no longer knows the content of this placeholder")
	errNotInSync           = errors.New("file is not in sync with the cluster")
	errNoOtherCopy         = errors.New("no other device has this version of the file")
	errNotLocal            = errors.New("file content is not local")
	errModifiedOnDisk      = errors.New("file was modified on disk and has not been scanned yet")
)

// onDemandState is the per-folder bookkeeping for on-demand files.
type onDemandState struct {
	// Shared by every instance of this folder: a folder restarts on any
	// configuration change, while hydrations and background finishers of
	// the previous instance may still be running on the same files.
	*hydrationState

	maintenancePending atomic.Bool
	started            time.Time

	pf    prefetchQueue
	crawl crawlTracker

	mut     sync.Mutex
	pinKey  string
	pins    *ignore.Matcher
	viewErr error
}

// hydrationState coordinates hydrations of a folder's files. It outlives
// folder restarts (model.hydrationState).
type hydrationState struct {
	// Hydrated files whose index entries still say "placeholder". They are
	// committed in batches, off the path of the waiting application.
	hydratedMut sync.Mutex
	hydrated    map[string][]byte // name -> blocks hash
	hydratedC   chan struct{}

	// indexMut orders our index updates with the puller's: it is held
	// while the puller commits a batch, and by our commits from validating
	// against the disk until written. Otherwise an update validated before
	// the puller replaced the file could land after the puller's record of
	// the replacement, and the index would describe a file that is gone.
	indexMut sync.Mutex
	// flushPuller asks the puller to commit its batch now.
	flushPuller chan struct{}

	// Hydrated files whose content is complete but not yet durable, by
	// inode. They stay placeholders until a background fsync (see
	// finishLater); accesses to them go ahead meanwhile.
	completeMut sync.Mutex
	complete    map[[2]uint64]struct{}
	finishing   sync.WaitGroup
	finishSem   chan struct{}

	// One hydration per file at a time; later callers wait for it.
	flightMut sync.Mutex
	flights   map[string]*hydrationFlight

	// Files being hydrated or made durable, which the scanner leaves alone.
	hydratingMut sync.Mutex
	hydrating    map[string]int
}

func newHydrationState() *hydrationState {
	return &hydrationState{
		hydrating:   make(map[string]int),
		hydrated:    make(map[string][]byte),
		hydratedC:   make(chan struct{}, 1),
		flushPuller: make(chan struct{}, 1),
		complete:    make(map[[2]uint64]struct{}),
		finishSem:   make(chan struct{}, maxFinishing),
		flights:     make(map[string]*hydrationFlight),
	}
}

func newOnDemandState(hs *hydrationState) *onDemandState {
	return &onDemandState{
		hydrationState: hs,
		started:        time.Now(),
		pf:             newPrefetchQueue(),
	}
}

// FileState is the on-demand state of a single file, as reported by the API.
type OnDemandFileState struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	State  string `json:"state"` // "local", "online-only" or "pinned"
	Pinned bool   `json:"pinned"`
}

func (f *sendReceiveFolder) liveConfig() config.FolderConfiguration {
	// Pin patterns and the cache budget change without a folder restart.
	if cfg, ok := f.model.cfg.Folder(f.folderID); ok {
		return cfg
	}
	return f.FolderConfiguration
}

func (f *sendReceiveFolder) absPath(name string) string {
	return filepath.Join(f.mtimefs.URI(), name)
}

// pinMatcher returns a matcher for the pin patterns (ignore-file syntax), or
// nil when nothing is pinned.
func (f *sendReceiveFolder) pinMatcher() *ignore.Matcher {
	patterns := f.liveConfig().PinPatterns
	key := strings.Join(patterns, "\n")
	f.od.mut.Lock()
	defer f.od.mut.Unlock()
	if key == f.od.pinKey && (f.od.pins != nil || key == "") {
		return f.od.pins
	}
	f.od.pinKey = key
	f.od.pins = nil
	if key == "" {
		return nil
	}
	m := ignore.New(f.mtimefs)
	if err := m.Parse(strings.NewReader(key), ".tetherpin"); err != nil {
		f.sl.Warn("Invalid pin patterns", slogutil.Error(err))
		return nil
	}
	f.od.pins = m
	return m
}

func (f *sendReceiveFolder) isPinned(name string) bool {
	m := f.pinMatcher()
	return m != nil && m.Match(name).IsIgnored()
}

func (f *sendReceiveFolder) setHydrating(name string, on bool) {
	f.od.hydratingMut.Lock()
	defer f.od.hydratingMut.Unlock()
	if on {
		f.od.hydrating[name]++
	} else if f.od.hydrating[name]--; f.od.hydrating[name] <= 0 {
		delete(f.od.hydrating, name)
	}
}

func (f *sendReceiveFolder) isHydrating(name string) bool {
	f.od.hydratingMut.Lock()
	defer f.od.hydratingMut.Unlock()
	_, ok := f.od.hydrating[name]
	return ok
}

func blocksHashOf(fi protocol.FileInfo) []byte {
	if len(fi.BlocksHash) > 0 {
		return fi.BlocksHash
	}
	if len(fi.Blocks) > 0 {
		return protocol.BlocksHash(fi.Blocks)
	}
	return nil
}

// handleVirtualFile brings the file on disk to the global version as a
// placeholder, without transferring any content.
func (f *sendReceiveFolder) handleVirtualFile(file protocol.FileInfo, dbUpdateChan chan<- dbUpdateJob, scanChan chan<- string) {
	f.evLogger.Log(events.ItemStarted, map[string]string{
		"folder": f.folderID,
		"item":   file.Name,
		"type":   "file",
		"action": "update",
	})

	err := f.writePlaceholder(file, dbUpdateChan, scanChan)
	if err != nil {
		f.newPullError(file.Name, fmt.Errorf("creating placeholder: %w", err))
	} else {
		f.sl.Debug("Created placeholder", file.LogAttr())
	}

	f.evLogger.Log(events.ItemFinished, map[string]any{
		"folder": f.folderID,
		"item":   file.Name,
		"error":  events.Error(err),
		"type":   "file",
		"action": "update",
	})
	f.queue.Done(file.Name)
}

func (f *sendReceiveFolder) writePlaceholder(file protocol.FileInfo, dbUpdateChan chan<- dbUpdateJob, scanChan chan<- string) error {
	curFile, hasCurFile, err := f.model.sdb.GetDeviceFile(f.folderID, protocol.LocalDeviceID, file.Name)
	if err != nil {
		return err
	}

	tempName := fs.TempName(file.Name)
	_ = f.mtimefs.Remove(tempName)
	err = f.inWritableDir(func(name string) error {
		fd, err := f.mtimefs.Create(name)
		if err != nil {
			return err
		}
		fd.Close()
		osf, err := os.OpenFile(f.absPath(name), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer osf.Close()
		return f.model.hsmMakePlaceholder(osf, file)
	}, tempName)
	if err != nil {
		_ = f.mtimefs.Remove(tempName)
		return err
	}

	file.LocalFlags = f.localFlags | protocol.FlagLocalVirtual
	if err := f.performFinish(file, curFile, hasCurFile, tempName, dbUpdateChan, scanChan); err != nil {
		_ = f.mtimefs.Remove(tempName)
		return err
	}
	return nil
}

// placeholderSource finds the index entry (with blocks) describing the
// content a placeholder stands for. The placeholder may have been moved, in
// which case its origin name differs from its current name.
func (f *sendReceiveFolder) placeholderSource(name, origin string, bh []byte) (protocol.FileInfo, bool) {
	src, _, ok := f.placeholderSourceLocal(name, origin, bh)
	return src, ok
}

// placeholderSourceLocal is placeholderSource, and also reports whether the
// entry found is the local one of name itself.
func (f *sendReceiveFolder) placeholderSourceLocal(name, origin string, bh []byte) (_ protocol.FileInfo, isLocal, ok bool) {
	names := []string{name}
	if origin != "" && origin != name {
		names = append(names, origin)
	}
	matches := func(fi protocol.FileInfo) bool {
		if fi.IsDeleted() || fi.Type != protocol.FileInfoTypeFile || len(fi.Blocks) == 0 && fi.Size > 0 {
			return false
		}
		return len(bh) == 0 || bytes.Equal(blocksHashOf(fi), bh)
	}
	for _, n := range names {
		if fi, ok, err := f.model.sdb.GetDeviceFile(f.folderID, protocol.LocalDeviceID, n); err == nil && ok && matches(fi) {
			return fi, n == name, true
		}
		if fi, ok, err := f.model.sdb.GetGlobalFile(f.folderID, n); err == nil && ok && matches(fi) {
			return fi, false, true
		}
	}
	return protocol.FileInfo{}, false, false
}

// placeholderChecker lets the scanner recognise placeholders.
type placeholderChecker struct {
	f *sendReceiveFolder
}

func (c placeholderChecker) Placeholder(name string) (scanner.Placeholder, bool) {
	f := c.f
	path := f.absPath(name)
	origin, bh, ok := hsm.ReadPlaceholder(path)
	if !ok {
		return scanner.Placeholder{}, false
	}
	// Keep hydrations of this file out while we look at it: the
	// modification time is not the file's while content is being written.
	f.od.flightMut.Lock()
	if _, busy := f.od.flights[name]; busy || f.isHydrating(name) {
		f.od.flightMut.Unlock()
		// Content is being written or made durable right now; leave it
		// alone.
		return scanner.Placeholder{}, true
	}
	f.od.flights[name] = &hydrationFlight{done: make(chan struct{})}
	f.od.flightMut.Unlock()
	defer func() {
		f.od.flightMut.Lock()
		fl := f.od.flights[name]
		delete(f.od.flights, name)
		f.od.flightMut.Unlock()
		close(fl.done)
	}()

	if hsm.Interrupted(path) {
		// A hydration failed without cleaning up, or we crashed during
		// one.
		if err := f.discardPartial(name); errors.Is(err, hsm.ErrModified) {
			// Written to after its download completed; an ordinary
			// file now, which the scanner treats as changed.
			f.sl.Info("Kept a file written to during its download", slogutil.FilePath(name))
			return scanner.Placeholder{}, false
		} else if err != nil {
			f.sl.Warn("Failed to reset interrupted hydration", slogutil.FilePath(name), slogutil.Error(err))
			return scanner.Placeholder{}, true
		}
		f.sl.Info("Reset interrupted hydration", slogutil.FilePath(name))
		if origin, bh, ok = hsm.ReadPlaceholder(path); !ok {
			return scanner.Placeholder{}, false
		}
	}
	src, ok := f.placeholderSource(name, origin, bh)
	if !ok && origin == name {
		if cur, healed := f.healUnknownPlaceholder(name); healed {
			src, ok = cur, true
		}
	}
	st, err := os.Lstat(path)
	if err != nil {
		return scanner.Placeholder{}, true
	}
	return scanner.Placeholder{Source: src, Known: ok, Size: st.Size(), ModTime: st.ModTime()}, true
}

// unknownPlaceholderGrace is how long a placeholder may stand for content
// the index does not know before it is reset: the puller records the
// placeholders it creates up to two seconds late.
const unknownPlaceholderGrace = 10 * time.Second

// healUnknownPlaceholder resets the placeholder at name to the local index
// entry, if that says the file has no local content and the placeholder
// stands for content the index does not know (and has for a while). No
// content is lost: neither has any. Otherwise the placeholder would be
// stuck: the scanner leaves unknown placeholders alone, and the puller
// does not replace a file that differs from the index. The caller must
// keep hydrations of name out.
func (f *sendReceiveFolder) healUnknownPlaceholder(name string) (protocol.FileInfo, bool) {
	ctime, err := hsm.Ctime(f.absPath(name))
	if err != nil || time.Since(ctime) < unknownPlaceholderGrace {
		return protocol.FileInfo{}, false
	}
	cur, ok, err := f.model.sdb.GetDeviceFile(f.folderID, protocol.LocalDeviceID, name)
	if err != nil || !ok || !cur.IsVirtual() || cur.IsDeleted() || cur.Type != protocol.FileInfoTypeFile || len(cur.Blocks) == 0 && cur.Size > 0 {
		return protocol.FileInfo{}, false
	}
	fd, restore, err := hsm.OpenPathForWrite(f.absPath(name))
	if err != nil {
		return protocol.FileInfo{}, false
	}
	defer fd.Close()
	defer restore()
	if err := hsm.Retarget(fd, cur.Size, name, blocksHashOf(cur), cur.ModTime()); err != nil {
		f.sl.Warn("Failed to reset placeholder of unknown content", slogutil.FilePath(name), slogutil.Error(err))
		return protocol.FileInfo{}, false
	}
	f.sl.Info("Reset placeholder of unknown content to the indexed version", slogutil.FilePath(name))
	return cur, true
}

func (f *sendReceiveFolder) discardPartial(name string) error {
	fd, restore, err := hsm.OpenPathForWrite(f.absPath(name))
	if err != nil {
		return err
	}
	defer fd.Close()
	defer restore()
	return hsm.Discard(fd)
}

var _ scanner.PlaceholderChecker = placeholderChecker{}

// hydrateKind says why a file is being hydrated.
type hydrateKind int

const (
	hydrateDemand    hydrateKind = iota // an application opened it and is waiting
	hydrateExplicit                     // pin or API request
	hydratePrefetch                     // speculative, from the prefetch queue
	hydrateLookahead                    // speculative, ahead of a tree walk (folder_crawl.go)
)

type hydrationFlight struct {
	done chan struct{}
	err  error
}

// hydrate fetches the content of the placeholder at name and writes it into
// dst, which may be the fanotify event fd or a file opened through the
// folder path. On success the file is a normal local file. Concurrent
// hydrations of the same name share one download.
func (k hydrateKind) speculative() bool {
	return k == hydratePrefetch || k == hydrateLookahead
}

func (f *sendReceiveFolder) hydrate(ctx context.Context, name string, dst *os.File, kind hydrateKind) error {
	for attempt := 0; attempt < 3; attempt++ {
		if f.isComplete(dst) {
			f.sl.Debug("Content complete, being made durable", slogutil.FilePath(name), "kind", kind)
			return nil
		}
		if _, _, ok := hsm.ReadPlaceholderFile(dst); !ok {
			return nil // already hydrated
		}
		f.od.flightMut.Lock()
		fl, busy := f.od.flights[name]
		if !busy {
			fl = &hydrationFlight{done: make(chan struct{})}
			f.od.flights[name] = fl
		}
		f.od.flightMut.Unlock()

		if !busy {
			if kind == hydrateDemand {
				f.prefetchSiblings(name)
			}
			fl.err = f.hydrateOnce(ctx, name, dst, kind)
			f.od.flightMut.Lock()
			delete(f.od.flights, name)
			f.od.flightMut.Unlock()
			close(fl.done)
			return fl.err
		}

		// Someone else (typically the prefetcher) is on it. Wait, then check
		// our own file: it may be a different inode (the placeholder was
		// replaced meanwhile) or the other attempt may have failed.
		select {
		case <-fl.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if kind.speculative() {
			return nil
		}
	}
	if _, _, ok := hsm.ReadPlaceholderFile(dst); ok {
		return fmt.Errorf("%s: could not hydrate: %w", name, syscall.EIO)
	}
	return nil
}

func (f *sendReceiveFolder) hydrateOnce(ctx context.Context, name string, dst *os.File, kind hydrateKind) error {
	// Checked again now that we hold the flight: the previous holder may
	// have completed the file after our caller looked (it marks the file
	// complete before releasing the flight).
	if f.isComplete(dst) {
		return nil
	}
	origin, bh, ok := hsm.ReadPlaceholderFile(dst)
	if !ok {
		return nil // already hydrated
	}

	f.setHydrating(name, true)
	defer f.setHydrating(name, false)

	src, srcIsLocal, known := f.placeholderSourceLocal(name, origin, bh)
	if !known && origin == name && !hsm.Unlinked(dst) {
		// The puller records the placeholders it creates in batches, up
		// to two seconds later.
		src, known = f.awaitPlaceholderSource(ctx, name, origin, bh, dst)
	}
	// Whether the index is to learn that the file was hydrated: not for an
	// unlinked placeholder.
	record := true
	for attempt := 0; ; attempt++ {
		err := fmt.Errorf("%s: %w", name, errNoPlaceholderSource)
		if known {
			superseded := f.supersededBy
			if srcIsLocal && attempt == 0 {
				// Saves looking up the same index entry again.
				superseded = func(name string, dst *os.File, bh []byte) (protocol.FileInfo, bool) {
					return f.supersededFrom(name, dst, src, bh)
				}
			}
			if newer, ok := superseded(name, dst, bh); ok && record {
				// The placeholder stands for a version that has been
				// replaced; the puller just has not caught up yet. Peers
				// may no longer have the old content. Nothing of it was
				// ever read (a placeholder is hydrated completely before
				// any access), so it can switch to the latest version,
				// like Windows' CF_OPERATION_TYPE_RESTART_HYDRATION or a
				// File Provider fetch without strict versioning. See
				// docs/research/stale-placeholders.md.
				if record, err = f.switchPlaceholder(name, dst, newer); err != nil {
					return err
				}
				f.sl.Debug("Hydrating newer version of superseded placeholder", slogutil.FilePath(name))
				src, bh = newer, blocksHashOf(newer)
			}
			recordIt := func() {}
			if record {
				bh := bh
				recordIt = func() { f.recordHydrated(name, bh) }
			}
			err = f.hydrateContent(ctx, name, src, dst, kind, recordIt)
			if err == nil {
				return nil
			}
		}
		if attempt > 0 || ctx.Err() != nil {
			return err
		}
		// A second chance, if the content can no longer be had because the
		// file changed meanwhile. A peer that finds its file no longer
		// matches the index rescans it: wait for the new version.
		if known && errors.Is(err, protocol.ErrNoSuchFile) {
			f.awaitNewerGlobal(ctx, name, src)
		}
		if hsm.Unlinked(dst) {
			// The puller replaced the placeholder while it was being
			// opened; the application holds the old, unlinked one. Any
			// complete version will do, and there is no index entry to
			// update: give it the latest.
			g, ok := f.latestGlobal(name)
			if !ok {
				return err
			}
			if err := hsm.Retarget(dst, g.Size, g.Name, blocksHashOf(g), g.ModTime()); err != nil {
				return err
			}
			src, bh, known, record = g, blocksHashOf(g), true, false
			continue
		}
		if _, ok := f.supersededBy(name, dst, bh); !ok {
			return err
		}
	}
}

// switchPlaceholder makes the placeholder dst stand for file, the newer
// global version of name, and records that in the index, as the puller
// would have: whether the content then arrives or not, the index describes
// what is on disk. recorded is false if dst was no longer at name.
func (f *sendReceiveFolder) switchPlaceholder(name string, dst *os.File, file protocol.FileInfo) (recorded bool, _ error) {
	f.od.indexMut.Lock()
	defer f.od.indexMut.Unlock()
	if err := hsm.Retarget(dst, file.Size, file.Name, blocksHashOf(file), file.ModTime()); err != nil {
		return false, err
	}
	if !f.isFileAt(name, dst) {
		return false, nil // replaced meanwhile; that replacement is recorded
	}
	file.LocalFlags = f.localFlags | protocol.FlagLocalVirtual
	file.Sequence = 0
	return true, f.updateLocals([]protocol.FileInfo{file})
}

// awaitPlaceholderSource waits a little for the index to learn about the
// placeholder dst.
func (f *sendReceiveFolder) awaitPlaceholderSource(ctx context.Context, name, origin string, bh []byte, dst *os.File) (protocol.FileInfo, bool) {
	select {
	case f.od.flushPuller <- struct{}{}:
	default:
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !hsm.Unlinked(dst) {
		select {
		case <-ctx.Done():
			return protocol.FileInfo{}, false
		case <-time.After(20 * time.Millisecond):
		}
		if src, ok := f.placeholderSource(name, origin, bh); ok {
			return src, true
		}
	}
	return protocol.FileInfo{}, false
}

// newerGlobalWait bounds how long a read waits for a peer to announce the
// version that replaced the one it no longer has. Peers rescan such a file
// at once, but there may be no newer version at all (e.g. the peer lost a
// conflict and moved its copy away).
const newerGlobalWait = 5 * time.Second

// awaitNewerGlobal waits a little for the global version of name to differ
// from src.
func (f *sendReceiveFolder) awaitNewerGlobal(ctx context.Context, name string, src protocol.FileInfo) {
	deadline := time.Now().Add(newerGlobalWait)
	for time.Now().Before(deadline) {
		if g, ok, err := f.model.sdb.GetGlobalFile(f.folderID, name); err != nil || !ok || !g.Version.Equal(src.Version) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// latestGlobal returns the global version of name, if it is a file whose
// content can be fetched.
func (f *sendReceiveFolder) latestGlobal(name string) (protocol.FileInfo, bool) {
	g, ok, err := f.model.sdb.GetGlobalFile(f.folderID, name)
	if err != nil || !ok || g.IsDeleted() || g.IsInvalid() || g.Type != protocol.FileInfoTypeFile || len(g.Blocks) == 0 && g.Size > 0 {
		return protocol.FileInfo{}, false
	}
	return g, true
}

// hydrateContent writes the content of src into the placeholder dst and
// turns it into a normal file.
func (f *sendReceiveFolder) hydrateContent(ctx context.Context, name string, src protocol.FileInfo, dst *os.File, kind hydrateKind, record func()) error {
	st, err := dst.Stat()
	if err != nil {
		return err
	}
	if st.Size() != src.Size {
		return fmt.Errorf("%s: placeholder size %d does not match index size %d: %w", name, st.Size(), src.Size, syscall.EIO)
	}
	started := time.Now()
	// The modification time from before any content was written, also by
	// an earlier attempt that was interrupted.
	mtime, err := hsm.BeginHydration(dst)
	if errors.Is(err, hsm.ErrModified) {
		// An earlier attempt completed and an application wrote to the
		// file since; the scanner picks the change up.
		f.sl.Info("Kept a file written to during its download", slogutil.FilePath(name))
		return nil
	} else if err != nil {
		return err
	}
	if err := f.fetchBlocks(ctx, name, src, dst); err != nil {
		// Leave the placeholder exactly as it was: writing the blocks we
		// got bumped its modification time, which the scanner would take
		// for a local change and announce as a new version.
		if derr := hsm.Discard(dst); derr != nil {
			f.sl.Warn("Failed to reset placeholder after failed hydration", slogutil.FilePath(name), slogutil.Error(derr))
		}
		return err
	}
	// Prefetched files get an access time older than their mtime: the cache
	// budget evicts them first if nobody uses them, and the first real read
	// refreshes it (relatime updates an atime older than mtime).
	atime := time.Now()
	if kind.speculative() {
		atime = time.Unix(1, 0)
	}
	// What the application may stat meanwhile. From here on, a write by
	// an application is recognised (hsm.Complete).
	if err := hsm.SetTimes(dst, mtime, atime); err != nil {
		return err
	}
	if err := hsm.Complete(dst); err != nil {
		return err
	}
	// The content must be durable before the file stops being a
	// placeholder, and before the index says it is local: otherwise a
	// power loss could leave a file that looks local but holds zeros.
	finish := func(fd *os.File) error {
		if err := fd.Sync(); err != nil {
			return err
		}
		if err := hsm.Finish(fd, mtime, atime); err != nil {
			return err
		}
		if kind == hydrateLookahead && f.crawlExpects(name) {
			// Marked until first opened, which tells the tree walk
			// tracker that the walker got here (hsmHandler.OpenedMarked).
			f.model.hsmKeepMarked(fd, f.folderID, name)
		} else {
			f.model.hsmUnmark(fd)
		}
		return nil
	}
	f.sl.Debug("Hydrated file", slogutil.FilePath(name), "size", src.Size, "duration", time.Since(started).Round(time.Millisecond))
	// Explicit requests (pins, `tether hydrate`) promise a local file when
	// they return; the others go on at once.
	if kind != hydrateExplicit && f.finishLater(name, dst, finish, record) {
		return nil
	}
	if err := finish(dst); err != nil {
		return err
	}
	record()
	return nil
}

// finishDelay holds hydrated files in the durability window for longer.
// Tests stretch it (TETHER_FINISH_DELAY) to exercise that window.
var finishDelay = func() time.Duration {
	d, _ := time.ParseDuration(os.Getenv("TETHER_FINISH_DELAY"))
	return d
}()

// maxFinishing bounds the hydrated files waiting to be made durable in the
// background; beyond it, hydration waits for its own fsync.
const maxFinishing = 256

// finishLater lets the application go on as soon as the content is
// complete, and makes the file durable in the background: an fsync costs
// several milliseconds per file (9 ms on btrfs), more than fetching a small
// file over a LAN. Until then the file stays a marked placeholder, with
// its hydration marker, that accesses go through unhindered (see
// isComplete); a crash meanwhile leaves an interrupted hydration, which is
// discarded as usual. record is called once the file is durable.
func (f *sendReceiveFolder) finishLater(name string, dst *os.File, finish func(*os.File) error, record func()) bool {
	key, ok := hsm.Key(dst)
	if !ok {
		return false
	}
	select {
	case f.od.finishSem <- struct{}{}:
	default:
		return false
	}
	dup, err := hsm.Dup(dst)
	if err != nil {
		<-f.od.finishSem
		return false
	}
	f.setComplete(key, true)
	// Keeps the scanner away until the file is durable.
	f.setHydrating(name, true)
	f.od.finishing.Add(1)
	go func() {
		defer f.od.finishing.Done()
		defer func() { <-f.od.finishSem }()
		defer dup.Close()
		defer f.setHydrating(name, false)
		defer f.setComplete(key, false)
		if finishDelay > 0 {
			time.Sleep(finishDelay)
		}
		if err := finish(dup); err != nil {
			// The content is not durable: back to a plain placeholder.
			f.sl.Warn("Failed to store a hydrated file; it stays online-only", slogutil.FilePath(name), slogutil.Error(err))
			if err := hsm.Discard(dup); errors.Is(err, hsm.ErrModified) {
				f.sl.Info("Kept a file written to during its download", slogutil.FilePath(name))
			} else if err != nil {
				f.sl.Warn("Failed to reset placeholder", slogutil.FilePath(name), slogutil.Error(err))
			}
			return
		}
		record()
	}()
	return true
}

func (f *sendReceiveFolder) setComplete(key [2]uint64, on bool) {
	f.od.completeMut.Lock()
	defer f.od.completeMut.Unlock()
	if on {
		f.od.complete[key] = struct{}{}
	} else {
		delete(f.od.complete, key)
	}
}

// isComplete reports whether dst's content is complete and being made
// durable in the background.
func (f *sendReceiveFolder) isComplete(dst *os.File) bool {
	key, ok := hsm.Key(dst)
	if !ok {
		return false
	}
	f.od.completeMut.Lock()
	defer f.od.completeMut.Unlock()
	_, ok = f.od.complete[key]
	return ok
}

// recordHydrated queues the index update for a hydrated file. It is
// batched: doing it inline costs a database transaction and event bus round
// trips per file, which dominated the latency of opening many small files.
// If we crash before the batch is committed, the scanner notices that the
// file is no longer a placeholder and fixes the index (see
// walker.walkRegular).
func (f *sendReceiveFolder) recordHydrated(name string, bh []byte) {
	f.od.hydratedMut.Lock()
	f.od.hydrated[name] = bh
	f.od.hydratedMut.Unlock()
	select {
	case f.od.hydratedC <- struct{}{}:
	default:
	}
}

// supersededBy returns the global version of name if it replaces the
// content of the placeholder dst (blocks hash bh) and nothing local stands
// in the way: dst is the file at name, it is the local version, and that
// is unmodified and older than the global one. The placeholder must also
// be able to take the global version's place as is (same permissions);
// otherwise it is left to the puller.
func (f *sendReceiveFolder) supersededBy(name string, dst *os.File, bh []byte) (protocol.FileInfo, bool) {
	cur, ok, err := f.model.sdb.GetDeviceFile(f.folderID, protocol.LocalDeviceID, name)
	if err != nil || !ok {
		return protocol.FileInfo{}, false
	}
	return f.supersededFrom(name, dst, cur, bh)
}

// supersededFrom is supersededBy with the local index entry of name, cur,
// already at hand.
func (f *sendReceiveFolder) supersededFrom(name string, dst *os.File, cur protocol.FileInfo, bh []byte) (protocol.FileInfo, bool) {
	if !cur.IsVirtual() || !bytes.Equal(blocksHashOf(cur), bh) {
		return protocol.FileInfo{}, false
	}
	g, ok := f.latestGlobal(name)
	if !ok {
		return protocol.FileInfo{}, false
	}
	if bytes.Equal(blocksHashOf(g), bh) || g.Version.Equal(cur.Version) || !g.Version.GreaterEqual(cur.Version) {
		return protocol.FileInfo{}, false
	}
	if !f.IgnorePerms && !g.NoPermissions && g.Permissions&0o777 != cur.Permissions&0o777 {
		return protocol.FileInfo{}, false
	}
	if !f.isFileAt(name, dst) {
		return protocol.FileInfo{}, false
	}
	return g, true
}

func (f *sendReceiveFolder) isFileAt(name string, fd *os.File) bool {
	a, err := fd.Stat()
	if err != nil {
		return false
	}
	b, err := os.Lstat(f.absPath(name))
	return err == nil && os.SameFile(a, b)
}

// awaitFinishing waits, at most for timeout, for hydrated files still being
// made durable. Those that are not by then remain interrupted hydrations.
func (f *sendReceiveFolder) awaitFinishing(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		f.od.finishing.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// hydratedCommitDelay batches index updates for hydrated files. Tests can
// stretch it (TETHER_HYDRATED_COMMIT_DELAY) to exercise crash recovery.
var hydratedCommitDelay = func() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("TETHER_HYDRATED_COMMIT_DELAY")); err == nil {
		return d
	}
	return 100 * time.Millisecond
}()

func (f *sendReceiveFolder) hydratedCommitter(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			f.awaitFinishing(5 * time.Second)
			_ = f.commitHydrated()
			return
		case <-f.od.hydratedC:
		}
		select {
		case <-ctx.Done():
		case <-time.After(hydratedCommitDelay):
		}
		if err := f.commitHydrated(); err != nil {
			f.sl.Warn("Failed to record hydrated files", slogutil.Error(err))
		}
	}
}

// commitHydrated records pending hydrations in the index. Each entry is
// re-validated: the file may have been replaced or evicted again since.
func (f *sendReceiveFolder) commitHydrated() error {
	f.od.hydratedMut.Lock()
	pending := f.od.hydrated
	f.od.hydrated = make(map[string][]byte)
	f.od.hydratedMut.Unlock()
	if len(pending) == 0 {
		return nil
	}

	f.od.indexMut.Lock()
	defer f.od.indexMut.Unlock()
	batch := make([]protocol.FileInfo, 0, len(pending))
	var bytes_ int64
	for name, bh := range pending {
		cur, ok, err := f.model.sdb.GetDeviceFile(f.folderID, protocol.LocalDeviceID, name)
		if err != nil {
			return err
		}
		if !ok || !cur.IsVirtual() || !bytes.Equal(blocksHashOf(cur), bh) {
			continue
		}
		if _, _, ph := hsm.ReadPlaceholder(f.absPath(name)); ph {
			continue // evicted again in the meantime
		}
		cur.LocalFlags &^= protocol.FlagLocalVirtual
		cur.Sequence = 0
		batch = append(batch, cur)
		bytes_ += cur.Size
	}
	if len(batch) == 0 {
		return nil
	}
	if err := f.updateLocals(batch); err != nil {
		return err
	}
	slog.Info("Hydrated files", f.LogAttr(), "files", len(batch), "bytes", bytes_)
	if f.liveConfig().CacheBudget.Value > 0 {
		f.scheduleOnDemandMaintenance()
	}
	return nil
}

type blockSource struct {
	Availability
	name string
}

func (f *sendReceiveFolder) blockSources(name string, src protocol.FileInfo, block protocol.BlockInfo) []blockSource {
	var res []blockSource
	for _, av := range f.model.blockAvailability(f.FolderConfiguration, src, block) {
		res = append(res, blockSource{av, src.Name})
	}
	if name != src.Name {
		// Peers may already know the file under its new name.
		if gf, ok, err := f.model.sdb.GetGlobalFile(f.folderID, name); err == nil && ok && bytes.Equal(blocksHashOf(gf), blocksHashOf(src)) {
			for _, av := range f.model.blockAvailability(f.FolderConfiguration, gf, block) {
				res = append(res, blockSource{av, name})
			}
		}
	}
	return res
}

func (f *sendReceiveFolder) fetchBlocks(ctx context.Context, name string, src protocol.FileInfo, dst *os.File) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	blocks := make(chan protocol.BlockInfo)
	errs := make(chan error, hydrationConcurrency)
	var wg sync.WaitGroup
	for range hydrationConcurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for block := range blocks {
				if err := f.fetchBlock(ctx, name, src, block, dst); err != nil {
					errs <- err
					cancel()
					return
				}
			}
		}()
	}
feed:
	for _, block := range src.Blocks {
		if block.IsEmpty() {
			continue // placeholders are sparse; holes read as zeros
		}
		select {
		case blocks <- block:
		case <-ctx.Done():
			break feed
		}
	}
	close(blocks)
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return err
	}
	return ctx.Err()
}

func (f *sendReceiveFolder) fetchBlock(ctx context.Context, name string, src protocol.FileInfo, block protocol.BlockInfo, dst *os.File) error {
	timeout := time.Duration(f.liveConfig().HydrationTimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	blockNo := int(block.Offset / int64(src.BlockSize()))
	var lastErr error = errNoDevice
	for {
		candidates := f.blockSources(name, src, block)
		for len(candidates) > 0 {
			avs := make([]Availability, len(candidates))
			for i, c := range candidates {
				avs[i] = c.Availability
			}
			i := activity.leastBusy(avs)
			if i < 0 {
				break
			}
			sel := candidates[i]
			candidates = slices.Delete(candidates, i, i+1)

			activity.using(sel.Availability)
			// A peer that silently went away must not leave the
			// application blocked until the connection times out.
			rctx, cancel := context.WithTimeout(ctx, time.Until(deadline))
			buf, err := f.model.RequestGlobal(rctx, sel.ID, f.folderID, sel.name, blockNo, block.Offset, block.Size, block.Hash, sel.FromTemporary)
			cancel()
			activity.done(sel.Availability)
			if err == nil {
				err = f.verifyBuffer(buf, block)
			}
			if err != nil {
				lastErr = err
				continue
			}
			_, err = dst.WriteAt(buf, block.Offset)
			return err
		}

		// Nobody could serve the block. Wait if a source is about to become
		// usable: right after start before any connection exists, or while
		// a device holding the file is connected but still exchanging
		// cluster config. Otherwise fail now rather than keep the
		// application waiting.
		starting := time.Since(f.od.started) < hydrationStartupGrace && !f.model.anyConnected()
		if !starting && !f.model.sourceConnecting(f.folderID, src.Name, name) || time.Now().After(deadline) {
			return fmt.Errorf("fetching %s at offset %d: %w", name, block.Offset, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// hydrateName hydrates a placeholder through the folder path (used for pins
// and explicit requests, not for application access).
func (f *sendReceiveFolder) hydrateName(ctx context.Context, name string, kind hydrateKind) error {
	fd, restore, err := hsm.OpenPathForWrite(f.absPath(name))
	if err != nil {
		return err
	}
	defer fd.Close()
	defer restore()
	return f.hydrate(ctx, name, fd, kind)
}

// evict turns a local, fully synced file back into a placeholder.
// evictBatch evicts files and records them in the index in batches: a
// transaction per file dominated bulk eviction (V8's 19,816 files took 299
// s). If we crash before a commit, the scanner notices that the files are
// placeholders and fixes the index (walker.walkPlaceholder).
type evictBatch struct {
	f       *sendReceiveFolder
	verify  bool // re-read every block first (see verifyAgainstIndex)
	pending []protocol.FileInfo
	n       int
	bytes   int64
}

const evictBatchSize = 1000

// cacheLowWaterPct is where cache budget eviction stops, in percent of the
// budget.
const cacheLowWaterPct = 80

func (b *evictBatch) evict(name string) error {
	fi, evicted, err := b.f.evictFile(name, b.verify)
	if err != nil || !evicted {
		return err
	}
	b.pending = append(b.pending, fi)
	b.n++
	b.bytes += fi.Size
	if len(b.pending) >= evictBatchSize {
		return b.flush()
	}
	return nil
}

// flush commits the pending index updates. Call it when done.
func (b *evictBatch) flush() error {
	if len(b.pending) == 0 {
		return nil
	}
	f := b.f
	f.od.indexMut.Lock()
	defer f.od.indexMut.Unlock()
	// Drop files replaced since they were evicted: their new index entry
	// may already be committed.
	valid := b.pending[:0]
	for _, fi := range b.pending {
		cur, ok, err := f.model.sdb.GetDeviceFile(f.folderID, protocol.LocalDeviceID, fi.Name)
		if err != nil {
			return err
		}
		if !ok || !cur.Version.Equal(fi.Version) || !bytes.Equal(blocksHashOf(cur), blocksHashOf(fi)) {
			continue
		}
		if _, bh, ph := hsm.ReadPlaceholder(f.absPath(fi.Name)); !ph || !bytes.Equal(bh, blocksHashOf(fi)) {
			continue
		}
		valid = append(valid, fi)
	}
	err := f.updateLocals(valid)
	b.pending = b.pending[:0]
	return err
}

func (b *evictBatch) log() {
	if b.n > 0 {
		b.f.sl.Info("Evicted file content", "files", b.n, "size", b.bytes)
	}
}

// evictFile turns name into a placeholder and returns its index entry, to be
// committed by the caller. evicted is false if it already was one.
func (f *sendReceiveFolder) evictFile(name string, verify bool) (_ protocol.FileInfo, evicted bool, _ error) {
	cur, ok, err := f.model.sdb.GetDeviceFile(f.folderID, protocol.LocalDeviceID, name)
	switch {
	case err != nil:
		return cur, false, err
	case !ok || cur.IsDeleted() || cur.Type != protocol.FileInfoTypeFile:
		return cur, false, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	case cur.IsVirtual():
		return cur, false, nil
	case cur.IsInvalid():
		return cur, false, fmt.Errorf("%s: %w", name, errNotInSync)
	}
	global, ok, err := f.model.sdb.GetGlobalFile(f.folderID, name)
	if err != nil {
		return cur, false, err
	}
	if !ok || !global.Version.Equal(cur.Version) {
		return cur, false, fmt.Errorf("%s: %w", name, errNotInSync)
	}
	// Somebody else must hold this exact version, or evicting it would
	// destroy the only copy.
	if devs, err := f.model.sdb.GetGlobalAvailability(f.folderID, name); err != nil {
		return cur, false, err
	} else if len(devs) == 0 {
		return cur, false, fmt.Errorf("%s: %w", name, errNoOtherCopy)
	}

	err = f.model.hsmEvict(f.folderID, name, cur, func(fd *os.File) error {
		if err := f.verifyAgainstIndex(fd, cur, verify); err != nil {
			f.ScheduleForceRescan(name)
			return err
		}
		return nil
	})
	if err != nil {
		return cur, false, fmt.Errorf("%s: %w", name, err)
	}

	cur.LocalFlags |= protocol.FlagLocalVirtual
	cur.Sequence = 0
	f.sl.Debug("Evicted file content", slogutil.FilePath(name), "size", cur.Size)
	return cur, true, nil
}

// verifyAgainstIndex checks, under the eviction lease, that the file on disk
// is what the index says: the same size and modification time, which is how
// the scanner decides a file is unchanged (a write that keeps both is
// invisible to syncing too). With full, every block is also re-read and
// hashed. Other systems evict on a cheap clean state as well (Lustre's data
// version, the Cloud Files in-sync bit); re-reading costs a full read of
// every evicted byte. See docs/research/eviction.md.
func (f *sendReceiveFolder) verifyAgainstIndex(fd *os.File, cur protocol.FileInfo, full bool) error {
	st, err := fd.Stat()
	if err != nil {
		return err
	}
	if st.Size() != cur.Size || !cur.ModTime().Equal(st.ModTime()) && f.modTimeWindow == 0 {
		return errModifiedOnDisk
	}
	if _, _, ph := hsm.ReadPlaceholderFile(fd); ph {
		return errNotLocal
	}
	if !full {
		return nil
	}
	buf := make([]byte, cur.BlockSize())
	for _, b := range cur.Blocks {
		n, err := fd.ReadAt(buf[:b.Size], b.Offset)
		if err != nil && n != b.Size {
			return err
		}
		if !scanner.Validate(buf[:b.Size], b.Hash) {
			return errModifiedOnDisk
		}
	}
	return nil
}

// forEachLocal calls fn for every local file entry under prefix ("" means
// the whole folder).
func (f *sendReceiveFolder) forEachLocal(prefix string, fn func(protocol.FileInfo) bool) error {
	prefix = strings.Trim(filepath.ToSlash(prefix), "/")
	it, errFn := f.model.sdb.AllLocalFilesWithPrefix(f.folderID, protocol.LocalDeviceID, prefix)
	for fi := range it {
		if fi.Type != protocol.FileInfoTypeFile || fi.IsDeleted() {
			continue
		}
		if !fn(fi) {
			break
		}
	}
	return errFn()
}

// hydratePinned makes sure every pinned file is local.
func (f *sendReceiveFolder) hydratePinned(ctx context.Context) error {
	m := f.pinMatcher()
	if m == nil {
		return nil
	}
	var names []string
	if err := f.forEachLocal("", func(fi protocol.FileInfo) bool {
		if fi.IsVirtual() && m.Match(fi.Name).IsIgnored() {
			names = append(names, fi.Name)
		}
		return true
	}); err != nil {
		return err
	}
	for _, name := range names {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := f.hydrateName(ctx, name, hydrateExplicit); err != nil {
			f.sl.Warn("Failed to hydrate pinned file", slogutil.FilePath(name), slogutil.Error(err))
		}
	}
	return f.commitHydrated()
}

// enforceCacheBudget evicts the least recently accessed unpinned local
// files until local content fits the budget.
func (f *sendReceiveFolder) enforceCacheBudget() error {
	budget := f.liveConfig().CacheBudget
	if budget.Value <= 0 {
		return nil
	}
	if err := f.commitHydrated(); err != nil {
		return err
	}
	limit := int64(budget.BaseValue())
	if budget.Percentage() {
		usage, err := f.mtimefs.Usage(".")
		if err != nil {
			return err
		}
		limit = int64(float64(usage.Total) * budget.Value / 100) //nolint:gosec
	}

	type cand struct {
		name  string
		size  int64
		atime time.Time
	}
	var used int64
	var cands []cand
	pins := f.pinMatcher()
	if err := f.forEachLocal("", func(fi protocol.FileInfo) bool {
		if fi.IsVirtual() {
			return true
		}
		used += fi.Size
		if pins != nil && pins.Match(fi.Name).IsIgnored() {
			return true
		}
		cands = append(cands, cand{fi.Name, fi.Size, fileAtime(f.absPath(fi.Name))})
		return true
	}); err != nil {
		return err
	}
	if used <= limit {
		return nil
	}
	// Evict down to a low watermark rather than just under the budget, so
	// that a folder near its budget is not cleaned in many small rounds
	// (cachefiles, CernVM-FS, SeaDrive and RobinHood all do this).
	target := limit / 100 * cacheLowWaterPct
	slices.SortFunc(cands, func(a, b cand) int { return a.atime.Compare(b.atime) })
	batch := &evictBatch{f: f}
	defer batch.log()
	for _, c := range cands {
		if used <= target {
			break
		}
		before := batch.n
		if err := batch.evict(c.name); err != nil {
			f.sl.Debug("Not evicting", slogutil.FilePath(c.name), slogutil.Error(err))
		}
		if batch.n > before {
			used -= c.size
		}
	}
	if err := batch.flush(); err != nil {
		return err
	}
	if used > limit {
		f.sl.Info("Cache budget exceeded; remaining files are pinned, in use or not yet synced", "used", used, "budget", limit)
	}
	return nil
}

// onDemandLoop runs periodic maintenance on the folder's own goroutine.
func (f *sendReceiveFolder) onDemandLoop(ctx context.Context) {
	go f.hydratedCommitter(ctx)
	for range max(f.PrefetchConcurrency, 1) {
		go f.prefetchWorker(ctx)
	}
	go f.crawlJanitor(ctx)
	t := time.NewTicker(onDemandMaintenanceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.scheduleOnDemandMaintenance()
		}
	}
}

func (f *sendReceiveFolder) scheduleOnDemandMaintenance() {
	if !f.od.maintenancePending.CompareAndSwap(false, true) {
		return // already queued
	}
	go func() {
		_ = f.doInSync(func(ctx context.Context) error {
			f.od.maintenancePending.Store(false)
			if err := f.getHealthErrorWithoutIgnores(); err != nil {
				return nil
			}
			if err := f.hydratePinned(ctx); err != nil {
				f.sl.Warn("Hydrating pinned files", slogutil.Error(err))
			}
			if err := f.enforceCacheBudget(); err != nil {
				f.sl.Warn("Enforcing cache budget", slogutil.Error(err))
			}
			return nil
		})
	}()
}

// OnDemandStatus lists files under prefix with their on-demand state.
func (f *sendReceiveFolder) onDemandStatus(prefix string) ([]OnDemandFileState, error) {
	// Downloads still being made durable count as local here.
	f.awaitFinishing(5 * time.Second)
	if err := f.commitHydrated(); err != nil {
		return nil, err
	}
	var res []OnDemandFileState
	pins := f.pinMatcher()
	err := f.forEachLocal(prefix, func(fi protocol.FileInfo) bool {
		st := OnDemandFileState{Name: fi.Name, Size: fi.Size, State: "local"}
		st.Pinned = pins != nil && pins.Match(fi.Name).IsIgnored()
		if fi.IsVirtual() {
			st.State = "online-only"
		} else if st.Pinned {
			st.State = "pinned"
		}
		res = append(res, st)
		return true
	})
	slices.SortFunc(res, func(a, b OnDemandFileState) int { return cmp.Compare(a.Name, b.Name) })
	return res, err
}

// evictPrefix evicts all unpinned local files under prefix; verify re-reads
// each one first (see verifyAgainstIndex).
func (f *sendReceiveFolder) evictPrefix(prefix string, verify bool) (int, error) {
	// Downloads still being made durable count as local here.
	f.awaitFinishing(5 * time.Second)
	if err := f.commitHydrated(); err != nil {
		return 0, err
	}
	var names []string
	pins := f.pinMatcher()
	if err := f.forEachLocal(prefix, func(fi protocol.FileInfo) bool {
		if !fi.IsVirtual() && (pins == nil || !pins.Match(fi.Name).IsIgnored()) {
			names = append(names, fi.Name)
		}
		return true
	}); err != nil {
		return 0, err
	}
	batch := &evictBatch{f: f, verify: verify}
	defer batch.log()
	var errs []error
	for _, name := range names {
		if err := batch.evict(name); err != nil {
			errs = append(errs, err)
		}
	}
	if err := batch.flush(); err != nil {
		errs = append(errs, err)
	}
	return batch.n, errors.Join(errs...)
}

// hydratePrefix hydrates all placeholders under prefix.
func (f *sendReceiveFolder) hydratePrefix(ctx context.Context, prefix string) (int, error) {
	var names []string
	if err := f.forEachLocal(prefix, func(fi protocol.FileInfo) bool {
		if fi.IsVirtual() {
			names = append(names, fi.Name)
		}
		return true
	}); err != nil {
		return 0, err
	}
	var n int
	var errs []error
	for _, name := range names {
		if err := f.hydrateName(ctx, name, hydrateExplicit); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	if err := f.commitHydrated(); err != nil {
		errs = append(errs, err)
	}
	return n, errors.Join(errs...)
}
