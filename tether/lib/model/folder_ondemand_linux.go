// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

package model

import (
	"syscall"
	"time"
)

// fileAtime returns the last access time, used to pick eviction victims.
func fileAtime(path string) time.Time {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return time.Time{}
	}
	return time.Unix(st.Atim.Sec, st.Atim.Nsec)
}

// processGroup returns the process group of pid, or pid itself if it has
// exited.
func processGroup(pid int) int {
	if pg, err := syscall.Getpgid(pid); err == nil && pg > 0 {
		return pg
	}
	return pid
}

// processGroupAlive reports whether the process group (or process) id has
// any members left.
func processGroupAlive(id int) bool {
	return syscall.Kill(-id, 0) != syscall.ESRCH || syscall.Kill(id, 0) != syscall.ESRCH
}

// fileKey returns the device and inode of path, as hsm.Key does for an open
// file.
func fileKey(path string) ([2]uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return [2]uint64{}, false
	}
	return [2]uint64{st.Dev, st.Ino}, true
}
