// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/model"
)

// unmountStaleOnDemandViews detaches the views the child had mounted. With
// the child gone, placeholders in them would read as zeros.
func unmountStaleOnDemandViews() {
	path := filepath.Join(filepath.Dir(locations.Get(locations.ConfigFile)), model.ViewsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, view := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if view == "" {
			continue
		}
		if err := unix.Unmount(view, unix.MNT_DETACH); err != nil && err != unix.EINVAL && err != unix.ENOENT {
			slog.Error("Failed to unmount stale on-demand view", slogutil.FilePath(view), slogutil.Error(err))
			continue
		}
		slog.Warn("Unmounted on-demand view left behind by the exited process", slogutil.FilePath(view))
	}
	_ = os.Remove(path)
}
