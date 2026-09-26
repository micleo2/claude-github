// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package hsm

import "context"

type accessorKey struct{}

// Accessor returns the process ID of the application whose access led to a
// Handler.Hydrate call, if there was one.
func Accessor(ctx context.Context) (pid int, ok bool) {
	pid, ok = ctx.Value(accessorKey{}).(int)
	return pid, ok
}

func withAccessor(ctx context.Context, pid int) context.Context {
	return context.WithValue(ctx, accessorKey{}, pid)
}

// OpenObserver is implemented by a Handler that wants to know when a file
// that is no longer a placeholder but still carries a mark is opened. The
// mark is removed at that access, so each such file is reported once. A
// Handler keeps a hydrated file marked (by not calling Unmark) to learn
// when an application first opens it, e.g. to see whether a prefetched
// file was used.
type OpenObserver interface {
	OpenedMarked(key [2]uint64, pid int)
}
