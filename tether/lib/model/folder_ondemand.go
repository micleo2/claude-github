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
	maintenancePending atomic.Bool
	started            time.Time

	// Hydrated files whose index entries still say "placeholder". They are
	// committed in batches, off the path of the waiting application.
	hydratedMut sync.Mutex
	hydrated    map[string][]byte // name -> blocks hash
	hydratedC   chan struct{}

	// One hydration per file at a time; later callers wait for it.
	flightMut sync.Mutex
	flights   map[string]*hydrationFlight

	pf prefetchQueue

	mut       sync.Mutex
	hydrating map[string]struct{}
	pinKey    string
	pins      *ignore.Matcher
	viewErr   error
}

func newOnDemandState() *onDemandState {
	return &onDemandState{
		hydrating: make(map[string]struct{}),
		started:   time.Now(),
		hydrated:  make(map[string][]byte),
		hydratedC: make(chan struct{}, 1),
		flights:   make(map[string]*hydrationFlight),
		pf:        newPrefetchQueue(),
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
	f.od.mut.Lock()
	defer f.od.mut.Unlock()
	if on {
		f.od.hydrating[name] = struct{}{}
	} else {
		delete(f.od.hydrating, name)
	}
}

func (f *sendReceiveFolder) isHydrating(name string) bool {
	f.od.mut.Lock()
	defer f.od.mut.Unlock()
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
			return fi, true
		}
		if fi, ok, err := f.model.sdb.GetGlobalFile(f.folderID, n); err == nil && ok && matches(fi) {
			return fi, true
		}
	}
	return protocol.FileInfo{}, false
}

// placeholderChecker lets the scanner recognise placeholders.
type placeholderChecker struct {
	f *sendReceiveFolder
}

func (c placeholderChecker) Placeholder(name string) (protocol.FileInfo, bool, bool) {
	origin, bh, ok := hsm.ReadPlaceholder(c.f.absPath(name))
	if !ok {
		return protocol.FileInfo{}, false, false
	}
	if c.f.isHydrating(name) {
		// Content is being written right now; leave it alone.
		return protocol.FileInfo{}, true, false
	}
	src, ok := c.f.placeholderSource(name, origin, bh)
	return src, true, ok
}

var _ scanner.PlaceholderChecker = placeholderChecker{}

// hydrateKind says why a file is being hydrated.
type hydrateKind int

const (
	hydrateDemand   hydrateKind = iota // an application opened it and is waiting
	hydrateExplicit                    // pin or API request
	hydratePrefetch                    // speculative, from the prefetch queue
)

type hydrationFlight struct {
	done chan struct{}
	err  error
}

// hydrate fetches the content of the placeholder at name and writes it into
// dst, which may be the fanotify event fd or a file opened through the
// folder path. On success the file is a normal local file. Concurrent
// hydrations of the same name share one download.
func (f *sendReceiveFolder) hydrate(ctx context.Context, name string, dst *os.File, kind hydrateKind) error {
	for attempt := 0; attempt < 3; attempt++ {
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
		if kind == hydratePrefetch {
			return nil
		}
	}
	if _, _, ok := hsm.ReadPlaceholderFile(dst); ok {
		return fmt.Errorf("%s: could not hydrate: %w", name, syscall.EIO)
	}
	return nil
}

func (f *sendReceiveFolder) hydrateOnce(ctx context.Context, name string, dst *os.File, kind hydrateKind) error {
	origin, bh, ok := hsm.ReadPlaceholderFile(dst)
	if !ok {
		return nil // already hydrated
	}

	f.setHydrating(name, true)
	defer f.setHydrating(name, false)

	src, ok := f.placeholderSource(name, origin, bh)
	if !ok {
		return fmt.Errorf("%s: %w", name, errNoPlaceholderSource)
	}
	st, err := dst.Stat()
	if err != nil {
		return err
	}
	if st.Size() != src.Size {
		return fmt.Errorf("%s: placeholder size %d does not match index size %d: %w", name, st.Size(), src.Size, syscall.EIO)
	}
	mtime := st.ModTime()

	started := time.Now()
	if err := f.fetchBlocks(ctx, name, src, dst); err != nil {
		return err
	}
	// The content must be durable before the index says it is local.
	if err := dst.Sync(); err != nil {
		return err
	}
	// Prefetched files get an access time older than their mtime: the cache
	// budget evicts them first if nobody uses them, and the first real read
	// refreshes it (relatime updates an atime older than mtime).
	atime := time.Now()
	if kind == hydratePrefetch {
		atime = time.Unix(1, 0)
	}
	if err := hsm.Finish(dst, mtime, atime); err != nil {
		return err
	}
	f.model.hsmUnmark(dst)

	// The file is complete and durable, so the application can go on. The
	// index update is batched: doing it inline costs a database transaction
	// and event bus round trips per file, which dominated the latency of
	// opening many small files. If we crash before the batch is committed,
	// the scanner notices that the file is no longer a placeholder and
	// fixes the index (see walker.walkRegular).
	f.od.hydratedMut.Lock()
	f.od.hydrated[name] = blocksHashOf(src)
	f.od.hydratedMut.Unlock()
	select {
	case f.od.hydratedC <- struct{}{}:
	default:
	}
	f.sl.Debug("Hydrated file", slogutil.FilePath(name), "size", src.Size, "duration", time.Since(started).Round(time.Millisecond))
	return nil
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
	pending []protocol.FileInfo
	n       int
	bytes   int64
}

const evictBatchSize = 1000

func (b *evictBatch) evict(name string) error {
	fi, evicted, err := b.f.evictFile(name)
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
	err := b.f.updateLocals(b.pending)
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
func (f *sendReceiveFolder) evictFile(name string) (_ protocol.FileInfo, evicted bool, _ error) {
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

	fd, restore, err := hsm.OpenPathForWrite(f.absPath(name))
	if err != nil {
		return cur, false, err
	}
	defer fd.Close()
	defer restore()
	// While the lease is held nobody else can open the file, so nothing can
	// change it between the check below and the eviction.
	if err := hsm.Lease(fd); err != nil {
		return cur, false, fmt.Errorf("%s: %w", name, err)
	}
	defer hsm.Unlease(fd) //nolint:errcheck

	if err := f.verifyAgainstIndex(fd, cur); err != nil {
		f.ScheduleForceRescan(name)
		return cur, false, fmt.Errorf("%s: %w", name, err)
	}
	if err := f.model.hsmMakePlaceholder(fd, cur); err != nil {
		return cur, false, fmt.Errorf("%s: %w", name, err)
	}

	cur.LocalFlags |= protocol.FlagLocalVirtual
	cur.Sequence = 0
	f.sl.Debug("Evicted file content", slogutil.FilePath(name), "size", cur.Size)
	return cur, true, nil
}

// verifyAgainstIndex checks that the file on disk is exactly what the index
// says, block by block.
func (f *sendReceiveFolder) verifyAgainstIndex(fd *os.File, cur protocol.FileInfo) error {
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
	slices.SortFunc(cands, func(a, b cand) int { return a.atime.Compare(b.atime) })
	batch := &evictBatch{f: f}
	defer batch.log()
	for _, c := range cands {
		if used <= limit {
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

// evictPrefix evicts all unpinned local files under prefix.
func (f *sendReceiveFolder) evictPrefix(prefix string) (int, error) {
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
	batch := &evictBatch{f: f}
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
