// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package config

import (
	"path/filepath"
	"testing"

	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"
)

// A folder accepted from a share offer on a client (defaults.folder has
// onDemand set) gets the chosen path as its view and a derived data path.
func TestOnDemandAcceptedFolderDerivesView(t *testing.T) {
	cfg := New(device1)
	cfg.Defaults.Folder.OnDemand = true

	fcfg := cfg.Defaults.Folder.Copy()
	fcfg.ID = "abcde-fghij"
	fcfg.Path = "/home/user/Docs"
	cfg.Folders = append(cfg.Folders, fcfg)
	if err := cfg.prepare(device1); err != nil {
		t.Fatal(err)
	}

	if !cfg.Defaults.Folder.OnDemand || cfg.Defaults.Folder.OnDemandView != "" {
		t.Errorf("defaults changed: onDemand=%v view=%q", cfg.Defaults.Folder.OnDemand, cfg.Defaults.Folder.OnDemandView)
	}
	f := cfg.Folders[0]
	if !f.OnDemand {
		t.Fatal("on-demand was disabled")
	}
	if f.OnDemandView != "/home/user/Docs" {
		t.Errorf("view = %q", f.OnDemandView)
	}
	if want := filepath.Join(locations.Get(locations.OnDemandData), "abcde-fghij"); f.Path != want {
		t.Errorf("path = %q, want %q", f.Path, want)
	}

	// Preparing again (every load and every change) is a no-op.
	if err := cfg.prepare(device1); err != nil {
		t.Fatal(err)
	}
	if g := cfg.Folders[0]; g.Path != f.Path || g.OnDemandView != f.OnDemandView {
		t.Errorf("second prepare changed paths: %q %q", g.Path, g.OnDemandView)
	}
}

// Explicitly configured views are left alone.
func TestOnDemandExplicitViewKept(t *testing.T) {
	f := FolderConfiguration{ID: "docs", Path: "/srv/lower", OnDemand: true, OnDemandView: "/home/user/Docs"}
	f.deriveOnDemandView("/state/ondemand")
	if f.Path != "/srv/lower" || f.OnDemandView != "/home/user/Docs" {
		t.Errorf("changed: path=%q view=%q", f.Path, f.OnDemandView)
	}
}

// A path that is already a derived data path is not turned into a view, and
// an unusable on-demand setting is never silently dropped: the folder must
// refuse to start rather than download everything.
func TestOnDemandNeverSilentlyDisabled(t *testing.T) {
	f := FolderConfiguration{ID: "docs", Path: "/state/ondemand/docs", OnDemand: true}
	f.deriveOnDemandView("/state/ondemand")
	if f.OnDemandView != "" || f.Path != "/state/ondemand/docs" {
		t.Errorf("derived from a data path: path=%q view=%q", f.Path, f.OnDemandView)
	}

	for _, fcfg := range []FolderConfiguration{
		{ID: "a", Path: "/x", Type: FolderTypeSendReceive, OnDemand: true},
		{ID: "b", Path: "/y", Type: FolderTypeReceiveOnly, OnDemand: true, OnDemandView: "/v"},
	} {
		fcfg.prepare(protocol.EmptyDeviceID, nil)
		if !fcfg.OnDemand {
			t.Errorf("%s: on-demand silently disabled", fcfg.ID)
		}
	}
}
