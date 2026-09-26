// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !linux

package model

import (
	"os"
	"time"
)

func fileAtime(path string) time.Time {
	if fi, err := os.Lstat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

func processGroup(pid int) int { return pid }

func processGroupAlive(int) bool { return false }

func fileKey(string) ([2]uint64, bool) { return [2]uint64{}, false }
