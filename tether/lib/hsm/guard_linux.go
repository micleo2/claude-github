// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

package hsm

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

//go:generate sh -c "clang -O2 -g -target bpf -c bpf/guard.bpf.c -o bpf/guard.bpf.o && llvm-strip -g bpf/guard.bpf.o"

//go:embed bpf/guard.bpf.o
var guardObject []byte

const (
	guardPin     = "placeholder-guard"
	bpfFSMagic   = 0xcafe4a11
	guardProgram = "tether_guard"
)

// InstallGuard attaches the placeholder guard (bpf/guard.bpf.c), a BPF LSM
// program that fails opens of placeholders with EIO whenever no fanotify
// group has them marked, i.e. while tether is stopped. It stays attached
// after this process exits: its link is pinned in a bpffs instance mounted
// at dir. Installing again replaces the running guard without a gap.
//
// Needs CAP_SYS_ADMIN, the bpf LSM (lsm=...,bpf) and Linux >= 6.8. A reboot
// removes the guard until tether starts again.
func InstallGuard(dir string) error {
	if err := mountBPFFS(dir); err != nil {
		return err
	}
	spec, err := ebpf.LoadCollectionSpecFromReader(bytes.NewReader(guardObject))
	if err != nil {
		return fmt.Errorf("placeholder guard: %w", err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return fmt.Errorf("load placeholder guard: %w", err)
	}
	defer coll.Close()
	l, err := link.AttachLSM(link.LSMOptions{Program: coll.Programs[guardProgram]})
	if err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("attach placeholder guard (is the bpf LSM enabled? see /sys/kernel/security/lsm): %w", err)
		}
		return fmt.Errorf("attach placeholder guard: %w", err)
	}
	defer l.Close() // the pin keeps it attached
	// Pin under a new name, then rename it over the old pin: the new guard
	// is attached before the old one is released. (bpffs rejects names
	// with dots.)
	pin := filepath.Join(dir, guardPin)
	next := pin + "-new"
	_ = os.Remove(next)
	if err := l.Pin(next); err != nil {
		return fmt.Errorf("pin placeholder guard: %w", err)
	}
	if err := os.Rename(next, pin); err != nil {
		_ = os.Remove(next)
		return fmt.Errorf("replace placeholder guard: %w", err)
	}
	return nil
}

// RemoveGuard detaches the guard installed at dir, if any.
func RemoveGuard(dir string) error {
	err := os.Remove(filepath.Join(dir, guardPin))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func mountBPFFS(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err == nil && st.Type == bpfFSMagic {
		return nil
	}
	if err := unix.Mount("bpf", dir, "bpf", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=0700"); err != nil {
		return fmt.Errorf("mount bpffs at %s: %w", dir, err)
	}
	return nil
}
