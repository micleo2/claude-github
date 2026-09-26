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
	"strconv"
	"strings"

	"github.com/syncthing/syncthing/internal/slogutil"
	"github.com/syncthing/syncthing/lib/hsm"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/model"
	"golang.org/x/sys/unix"
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
		if err := hsm.UnmountView(view); err != nil {
			slog.Error("Failed to unmount stale on-demand view", slogutil.FilePath(view), slogutil.Error(err))
			continue
		}
		slog.Warn("Unmounted on-demand view left behind by the exited process", slogutil.FilePath(view))
	}
	_ = os.Remove(path)
}

// groupFDName names the fanotify group in the service manager's fd store.
const groupFDName = "tether-hsm-group"

// onDemandGroup creates the fanotify group that every sync process inherits
// (hsm.NewGroup), or returns nil where on-demand files are unavailable (old
// kernel, no CAP_SYS_ADMIN).
//
// Under systemd with FileDescriptorStoreMax=, a copy of the group is kept
// in the service's fd store, so that it survives this process too: if the
// monitor is killed, accesses to placeholders keep waiting (rather than
// being allowed by the kernel and reading zeros) until the service is
// restarted and takes the group back from the store.
func onDemandGroup() *os.File {
	if f := fdFromStore(groupFDName); f != nil {
		slog.Info("Resuming the on-demand group kept by the service manager")
		return f
	}
	fd, err := hsm.NewGroup()
	if err != nil {
		slog.Debug("No shared on-demand group", slogutil.Error(err))
		return nil
	}
	f := os.NewFile(uintptr(fd), "fanotify-group")
	if ok, err := sdNotify("FDSTORE=1\nFDNAME="+groupFDName, int(f.Fd())); err != nil {
		slog.Warn("Failed to hand the on-demand group to the service manager", slogutil.Error(err))
	} else if ok {
		slog.Debug("Handed the on-demand group to the service manager's fd store")
	}
	return f
}

// closeOnDemandGroup releases the group for good, when tether stops. It
// fails the accesses still waiting on it first: the kernel would allow
// them when the group closes, and the placeholders would read as zeros.
func closeOnDemandGroup(group *os.File) {
	if n := hsm.DenyPending(int(group.Fd())); n > 0 {
		slog.Warn("Stopped with downloads in progress; they fail with EIO", "count", n)
	}
	_, _ = sdNotify("FDSTOREREMOVE=1\nFDNAME="+groupFDName, -1)
	group.Close()
}

// fdFromStore takes the fd named name from those passed by the service
// manager (sd_listen_fds with names), if any. The variables are removed so
// that child processes do not see them.
func fdFromStore(name string) *os.File {
	pid, count, names := os.Getenv("LISTEN_PID"), os.Getenv("LISTEN_FDS"), os.Getenv("LISTEN_FDNAMES")
	for _, v := range []string{"LISTEN_PID", "LISTEN_PIDFDID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		os.Unsetenv(v)
	}
	if pid != strconv.Itoa(os.Getpid()) {
		return nil
	}
	n, err := strconv.Atoi(count)
	if err != nil {
		return nil
	}
	const listenFDsStart = 3
	var found *os.File
	for i, fdName := range strings.Split(names, ":") {
		if i >= n {
			break
		}
		fd := listenFDsStart + i
		unix.CloseOnExec(fd)
		if fdName == name && found == nil {
			found = os.NewFile(uintptr(fd), name)
		}
	}
	return found
}

// sdNotify sends state to the service manager, with fd attached if it is
// not negative. ok is false if there is no service manager to tell.
func sdNotify(state string, fd int) (ok bool, _ error) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return false, nil
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	s, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(s)
	var oob []byte
	if fd >= 0 {
		oob = unix.UnixRights(fd)
	}
	return true, unix.Sendmsg(s, []byte(state), oob, &unix.SockaddrUnix{Name: addr}, 0)
}

// installOnDemandGuard keeps placeholders from reading as zeros while tether
// is stopped (hsm.InstallGuard); the group only covers the time it runs.
func installOnDemandGuard() {
	dir := filepath.Join(filepath.Dir(locations.Get(locations.OnDemandData)), "guard")
	if err := hsm.InstallGuard(dir); err != nil {
		slog.Warn("Placeholder guard unavailable: online-only files read as zeros while tether is stopped", slogutil.Error(err))
		return
	}
	slog.Info("Placeholder guard active", slogutil.FilePath(dir))
}
