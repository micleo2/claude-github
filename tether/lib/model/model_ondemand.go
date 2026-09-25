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

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/hsm"
	"github.com/syncthing/syncthing/lib/locations"
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
	OnDemandEvict(folder, path string) (int, error)
	OnDemandHydrate(ctx context.Context, folder, path string) (int, error)
}

var _ OnDemander = (*model)(nil)

type modelOnDemand struct {
	mut    sync.Mutex
	l      *hsm.Listener
	cancel context.CancelFunc
	views  map[string]string // folder ID -> view path
}

type hsmHandler struct{ m *model }

func (h hsmHandler) Hydrate(ctx context.Context, folder, name string, f *os.File) error {
	sr, err := h.m.onDemandFolder(folder)
	if err != nil {
		return err
	}
	return sr.hydrate(ctx, name, f)
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

func (m *model) hsmForget(fd *os.File) {
	m.od.mut.Lock()
	l := m.od.l
	m.od.mut.Unlock()
	if l != nil {
		if err := l.Forget(fd); err != nil {
			slog.Warn("Failed to re-arm on-demand interception", slogutil.Error(err))
		}
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

func (m *model) OnDemandEvict(folder, path string) (int, error) {
	sr, err := m.onDemandFolder(folder)
	if err != nil {
		return 0, err
	}
	var n int
	err = sr.doInSync(func(context.Context) error {
		var err error
		n, err = sr.evictPrefix(path)
		return err
	})
	return n, err
}

func (m *model) OnDemandHydrate(ctx context.Context, folder, path string) (int, error) {
	sr, err := m.onDemandFolder(folder)
	if err != nil {
		return 0, err
	}
	return sr.hydratePrefix(ctx, path)
}
