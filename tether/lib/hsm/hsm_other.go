// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !linux

package hsm

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"syscall"
	"time"
)

var errUnsupported = errors.New("on-demand files require Linux")

const (
	XattrState      = "user.tether.state"
	StateVirtual    = "virtual"
	XattrOrigin     = "user.tether.origin"
	XattrBlocksHash = "user.tether.bh"
	XattrPrefix     = "user.tether."
	GroupEnv        = "TETHER_HSM_GROUP_FD"
)

type Handler interface {
	Hydrate(ctx context.Context, folder, name string, f *os.File) error
}

type Policy func(folder string, pid int, exe string) syscall.Errno

type View struct {
	Folder, Lower, Path string
}

type Listener struct{}

func Supported() error { return errUnsupported }

func New(Handler, Policy, *slog.Logger) (*Listener, error) { return nil, errUnsupported }

func (*Listener) AddView(*View) error     { return errUnsupported }
func (*Listener) RemoveView(string) error { return errUnsupported }
func (*Listener) Close() error            { return nil }
func (*Listener) MakePlaceholder(*os.File, int64, string, []byte) error {
	return errUnsupported
}
func (*Listener) Unmark(*os.File)                 {}
func (*Listener) MarkTree(string) (int, error)    { return 0, errUnsupported }
func (*Listener) Serve(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

func IsVirtual(*os.File) bool                             { return false }
func ReadPlaceholder(string) (string, []byte, bool)       { return "", nil, false }
func ReadPlaceholderFile(*os.File) (string, []byte, bool) { return "", nil, false }
func Finish(*os.File, time.Time, time.Time) error         { return errUnsupported }
func MarkVirtual(*os.File, int64, string, []byte) error   { return errUnsupported }
func Lease(*os.File) error                                { return errUnsupported }
func OpenPathForWrite(string) (*os.File, func(), error)   { return nil, nil, errUnsupported }
func Unlease(*os.File) error                              { return errUnsupported }
