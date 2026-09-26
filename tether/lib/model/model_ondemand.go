// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/hsm"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"
)

// ViewsFileName lists the on-demand views currently mounted, one per line.
// The monitor process unmounts them if the sync process dies, so that
// placeholders are never readable without the listener (they would read as
// zeros).
const ViewsFileName = "ondemand-views"

// Executables that index or copy entire trees. Letting them hydrate
// everything would defeat the point of on-demand files.
var defaultHydrationDenyExes = []string{
	"tracker-miner-fs-3", "tracker-extract-3", "localsearch-3", "localsearch-extractor-3",
	"baloo_file", "baloo_file_extractor", "updatedb", "plocate", "updatedb.plocate", "mlocate",
}

// OnDemander is implemented by the model; the API uses it for the
// /rest/ondemand endpoints.
type OnDemander interface {
	OnDemandStatus(folder, prefix string) ([]OnDemandFileState, error)
	OnDemandPin(folder, path string) error
	OnDemandUnpin(folder, path string) error
	OnDemandEvict(folder, path string, verify bool) (int, error)
	OnDemandHydrate(ctx context.Context, folder, path string) (int, error)
}

var _ OnDemander = (*model)(nil)

// A device that just lost its connection is likely to be back shortly (a
// connection being replaced, a brief network blip); hydrations wait for it
// this long instead of failing at once.
const reconnectGrace = 10 * time.Second

type modelOnDemand struct {
	mut    sync.Mutex
	l      *hsm.Listener
	cancel context.CancelFunc
	views  map[string]string // folder ID -> view path

	discMut     sync.Mutex
	disconnects map[protocol.DeviceID]time.Time

	// Per folder ID, across folder restarts (see onDemandState).
	hydrations map[string]*hydrationState

	// Files prefetched ahead of a tree walk, kept marked until opened.
	keptMut sync.Mutex
	kept    map[[2]uint64]keptMark
}

type keptMark struct{ folder, name string }

// maxKeptMarks bounds the files kept marked after a lookahead prefetch.
// Beyond it, they are unmarked at once and the walk tracker does not see
// them opened.
const maxKeptMarks = 1 << 16

// hydrationState returns the hydration bookkeeping of the folder, which
// every instance of it shares.
func (m *model) hydrationState(folder string) *hydrationState {
	m.od.mut.Lock()
	defer m.od.mut.Unlock()
	if m.od.hydrations == nil {
		m.od.hydrations = make(map[string]*hydrationState)
	}
	hs, ok := m.od.hydrations[folder]
	if !ok {
		hs = newHydrationState()
		m.od.hydrations[folder] = hs
	}
	return hs
}

func (o *modelOnDemand) noteDisconnect(dev protocol.DeviceID) {
	o.discMut.Lock()
	defer o.discMut.Unlock()
	if o.disconnects == nil {
		o.disconnects = make(map[protocol.DeviceID]time.Time)
	}
	o.disconnects[dev] = time.Now()
}

func (o *modelOnDemand) recentlyDisconnected(dev protocol.DeviceID) bool {
	o.discMut.Lock()
	defer o.discMut.Unlock()
	t, ok := o.disconnects[dev]
	return ok && time.Since(t) < reconnectGrace
}

type hsmHandler struct{ m *model }

func (h hsmHandler) Hydrate(ctx context.Context, folder, name string, f *os.File) error {
	sr, err := h.m.onDemandFolder(folder)
	if err != nil {
		return err
	}
	if pid, ok := hsm.Accessor(ctx); ok {
		sr.crawlTouch(pid, name, sr.isComplete(f))
	}
	return sr.hydrate(ctx, name, f, hydrateDemand)
}

// OpenedMarked is called when a file we kept marked after prefetching it
// ahead of a tree walk is opened (see hsmKeepMarked).
func (h hsmHandler) OpenedMarked(key [2]uint64, pid int) {
	h.m.od.keptMut.Lock()
	k, ok := h.m.od.kept[key]
	delete(h.m.od.kept, key)
	h.m.od.keptMut.Unlock()
	if !ok {
		return
	}
	if sr, err := h.m.onDemandFolder(k.folder); err == nil {
		sr.crawlTouch(pid, k.name, true)
	}
}

func (m *model) hydrationPolicy(folder string, _ int, exe string) syscall.Errno {
	base := filepath.Base(exe)
	deny := defaultHydrationDenyExes
	if cfg, ok := m.cfg.Folder(folder); ok {
		deny = append(slices.Clone(deny), cfg.HydrationDenyExes...)
	}
	for _, d := range deny {
		if d == base || d == exe {
			return syscall.EPERM
		}
	}
	return 0
}

func (m *model) hsmAddView(cfg config.FolderConfiguration) error {
	m.od.mut.Lock()
	defer m.od.mut.Unlock()
	if m.od.l == nil {
		l, err := hsm.New(hsmHandler{m}, m.hydrationPolicy, slog.Default())
		if err != nil {
			return fmt.Errorf("%w (on-demand files need Linux >= 6.14 and CAP_SYS_ADMIN)", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			if err := l.Serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("On-demand listener stopped", slogutil.Error(err))
			}
		}()
		m.od.l, m.od.cancel = l, cancel
	}
	if cfg.OnDemandView == "" {
		return errors.New("no view path (onDemandView) configured")
	}
	lower := cfg.Filesystem().URI()
	view := filepath.Clean(cfg.OnDemandView)
	if view == lower || strings.HasPrefix(view, lower+string(filepath.Separator)) || strings.HasPrefix(lower, view+string(filepath.Separator)) {
		return fmt.Errorf("view %s must not overlap the folder path %s", view, lower)
	}
	if err := m.od.l.AddView(&hsm.View{Folder: cfg.ID, Lower: lower, Path: view}); err != nil {
		return err
	}
	m.od.views[cfg.ID] = view
	m.writeViewsFileLocked()
	return nil
}

func (m *model) hsmRemoveView(cfg config.FolderConfiguration) {
	m.od.mut.Lock()
	defer m.od.mut.Unlock()
	view, ok := m.od.views[cfg.ID]
	if !ok || m.od.l == nil {
		return
	}
	delete(m.od.views, cfg.ID)
	if err := m.od.l.RemoveView(view); err != nil {
		slog.Warn("Failed to unmount on-demand view", cfg.LogAttr(), slogutil.Error(err))
	}
	m.writeViewsFileLocked()
}

// hsmMakePlaceholder turns fd into a marked placeholder. Without the
// listener it fails: an unmarked placeholder reads as zeros.
func (m *model) hsmMakePlaceholder(fd *os.File, file protocol.FileInfo) error {
	m.od.mut.Lock()
	l := m.od.l
	m.od.mut.Unlock()
	if l == nil {
		return errors.New("on-demand listener is not running")
	}
	return l.MakePlaceholder(fd, file.Size, file.Name, blocksHashOf(file))
}

// hsmEvict turns the local file name into a placeholder for file, if check
// approves of the content (see hsm.Listener.Evict).
func (m *model) hsmEvict(folder, name string, file protocol.FileInfo, check func(*os.File) error) error {
	m.od.mut.Lock()
	l := m.od.l
	m.od.mut.Unlock()
	if l == nil {
		return errors.New("on-demand listener is not running")
	}
	return l.Evict(folder, name, file.Size, file.Name, blocksHashOf(file), check)
}

func (m *model) hsmUnmark(fd *os.File) {
	m.od.mut.Lock()
	l := m.od.l
	m.od.mut.Unlock()
	if l != nil {
		l.Unmark(fd)
	}
}

// hsmKeepMarked keeps the mark of a hydrated file, so that its first open
// is reported to OpenedMarked (and the mark removed then).
func (m *model) hsmKeepMarked(fd *os.File, folder, name string) {
	key, ok := hsm.Key(fd)
	m.od.keptMut.Lock()
	if ok && len(m.od.kept) < maxKeptMarks {
		if m.od.kept == nil {
			m.od.kept = make(map[[2]uint64]keptMark)
		}
		m.od.kept[key] = keptMark{folder, name}
		m.od.keptMut.Unlock()
		return
	}
	m.od.keptMut.Unlock()
	m.hsmUnmark(fd)
}

// hsmUnmarkUnused removes the mark of a file we kept marked (see
// hsmKeepMarked) that nobody opened.
func (m *model) hsmUnmarkUnused(folder, name string, key [2]uint64) {
	m.od.keptMut.Lock()
	k, ok := m.od.kept[key]
	if ok && k.folder == folder && k.name == name {
		delete(m.od.kept, key)
	}
	m.od.keptMut.Unlock()
	m.od.mut.Lock()
	l := m.od.l
	m.od.mut.Unlock()
	if l != nil {
		l.UnmarkLocal(folder, name)
	}
}

func (m *model) hsmClose() {
	m.od.mut.Lock()
	defer m.od.mut.Unlock()
	if m.od.l == nil {
		return
	}
	m.od.cancel()
	m.od.l.Close()
	m.od.l = nil
	clear(m.od.views)
	m.writeViewsFileLocked()
}

func (m *model) writeViewsFileLocked() {
	path := filepath.Join(filepath.Dir(locations.Get(locations.ConfigFile)), ViewsFileName)
	if len(m.od.views) == 0 {
		_ = os.Remove(path)
		return
	}
	var b strings.Builder
	for _, v := range m.od.views {
		b.WriteString(v)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		slog.Warn("Failed to record on-demand views", slogutil.Error(err))
	}
}

// sourceConnecting reports whether a device that has any of the named
// files is connected but not yet usable as a source for the folder.
func (m *model) sourceConnecting(folder string, names ...string) bool {
	for _, name := range names {
		devs, err := m.sdb.GetGlobalAvailability(folder, name)
		if err != nil {
			continue
		}
		m.mut.RLock()
		for _, dev := range devs {
			if _, ok := m.deviceConnIDs[dev]; ok && m.remoteFolderStates[dev][folder] != remoteFolderValid {
				m.mut.RUnlock()
				return true
			}
		}
		m.mut.RUnlock()
		for _, dev := range devs {
			if devCfg, ok := m.cfg.Device(dev); ok && !devCfg.Paused && m.od.recentlyDisconnected(dev) {
				return true
			}
		}
	}
	return false
}

func (m *model) anyConnected() bool {
	m.mut.RLock()
	defer m.mut.RUnlock()
	return len(m.connections) > 0
}

func (m *model) onDemandFolder(folder string) (*sendReceiveFolder, error) {
	runner, ok := m.folderRunners.Get(folder)
	if !ok {
		return nil, fmt.Errorf("folder %q: %w", folder, ErrFolderMissing)
	}
	sr, ok := runner.(*sendReceiveFolder)
	if !ok || !sr.OnDemand {
		return nil, fmt.Errorf("folder %q does not use on-demand files", folder)
	}
	return sr, nil
}

func (m *model) OnDemandStatus(folder, prefix string) ([]OnDemandFileState, error) {
	sr, err := m.onDemandFolder(folder)
	if err != nil {
		return nil, err
	}
	return sr.onDemandStatus(prefix)
}

func pinPattern(path string) string {
	return "/" + strings.Trim(filepath.ToSlash(path), "/")
}

func (m *model) OnDemandPin(folder, path string) error {
	sr, err := m.onDemandFolder(folder)
	if err != nil {
		return err
	}
	pat := pinPattern(path)
	w, err := m.cfg.Modify(func(cfg *config.Configuration) {
		for i := range cfg.Folders {
			if cfg.Folders[i].ID == folder && !slices.Contains(cfg.Folders[i].PinPatterns, pat) {
				cfg.Folders[i].PinPatterns = append(cfg.Folders[i].PinPatterns, pat)
			}
		}
	})
	if err != nil {
		return err
	}
	w.Wait()
	return sr.doInSync(sr.hydratePinned)
}

func (m *model) OnDemandUnpin(folder, path string) error {
	if _, err := m.onDemandFolder(folder); err != nil {
		return err
	}
	pat := pinPattern(path)
	w, err := m.cfg.Modify(func(cfg *config.Configuration) {
		for i := range cfg.Folders {
			if cfg.Folders[i].ID == folder {
				cfg.Folders[i].PinPatterns = slices.DeleteFunc(cfg.Folders[i].PinPatterns, func(p string) bool { return p == pat })
			}
		}
	})
	if err != nil {
		return err
	}
	w.Wait()
	return nil
}

func (m *model) OnDemandEvict(folder, path string, verify bool) (int, error) {
	sr, err := m.onDemandFolder(folder)
	if err != nil {
		return 0, err
	}
	var n int
	var evictErr error
	err = sr.doInSync(func(context.Context) error {
		// Refusals (a file in use, or not in sync) are for the caller;
		// they do not put the folder into an error state.
		n, evictErr = sr.evictPrefix(path, verify)
		return nil
	})
	if err != nil {
		return n, err
	}
	return n, evictErr
}

func (m *model) OnDemandHydrate(ctx context.Context, folder, path string) (int, error) {
	sr, err := m.onDemandFolder(folder)
	if err != nil {
		return 0, err
	}
	return sr.hydratePrefix(ctx, path)
}
