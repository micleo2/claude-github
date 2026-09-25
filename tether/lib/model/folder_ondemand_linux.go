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
