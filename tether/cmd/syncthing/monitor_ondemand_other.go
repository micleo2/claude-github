// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !linux

package main

import "os"

func unmountStaleOnDemandViews() {}

func onDemandGroup() *os.File { return nil }

func closeOnDemandGroup(*os.File) {}

func installOnDemandGuard() {}
