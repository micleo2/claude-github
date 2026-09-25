// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

// Package hsm implements on-demand hydration of placeholder files using
// fanotify pre-content events (Linux >= 6.14).
//
// A folder's real directory (the "lower" path) is used by the sync engine.
// Users see it through a bind mount (the "view"), and only the view carries
// the fanotify mark, so the engine's own I/O never generates events. The view
// is marked before it becomes visible (bind at a private staging path, mark,
// then move-mount into place) so there is no window in which placeholders can
// be read as zeros.
//
// A placeholder is a sparse file of the right size and mtime whose xattr
// user.tether.state is "virtual". Opening or accessing it blocks the caller
// until the Handler has written the full content through the event fd.
package hsm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// XattrState holds "virtual" on placeholders. It is a fast-path hint;
	// the sync database is authoritative.
	XattrState   = "user.tether.state"
	StateVirtual = "virtual"

	eventMask  = unix.FAN_OPEN_PERM | unix.FAN_PRE_ACCESS
	ignoreMask = eventMask
)

// Handler supplies file content.
type Handler interface {
	// Hydrate writes the complete content of the file at name (relative to
	// the view's folder) into f and returns nil, or returns an error whose
	// errno (if any) is reported to the blocked application.
	Hydrate(ctx context.Context, folder, name string, f *os.File) error
}

// Policy decides whether a process may trigger hydration. Returning a
// non-zero errno denies the access with that errno.
type Policy func(pid int, exe string) unix.Errno

// View is a marked bind mount of a folder.
type View struct {
	Folder string // folder ID
	Lower  string // real directory used by the sync engine
	Path   string // user-visible mount point
}

type Listener struct {
	fd      int
	handler Handler
	policy  Policy
	log     *slog.Logger

	mu    sync.Mutex
	views []*View

	flights sync.Map // fileKey -> *flight

	closeOnce sync.Once
	closeErr  error
}

type fileKey struct {
	dev, ino uint64
	gen      int
}

type flight struct {
	done chan struct{}
	err  error
}

// Supported reports whether the running kernel accepts pre-content marks.
func Supported() error {
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_PRE_CONTENT|unix.FAN_CLOEXEC, unix.O_RDONLY)
	if err != nil {
		return fmt.Errorf("fanotify_init(FAN_CLASS_PRE_CONTENT): %w", err)
	}
	unix.Close(fd)
	return nil
}

func New(handler Handler, policy Policy, log *slog.Logger) (*Listener, error) {
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_PRE_CONTENT|unix.FAN_CLOEXEC|unix.FAN_UNLIMITED_QUEUE,
		unix.O_RDWR|unix.O_LARGEFILE)
	if err != nil {
		return nil, fmt.Errorf("fanotify_init: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Listener{fd: fd, handler: handler, policy: policy, log: log}, nil
}

// AddView mounts lower at path with the fanotify mark in place before the
// mount is reachable at path.
func (l *Listener) AddView(v *View) error {
	if err := os.MkdirAll(v.Path, 0o755); err != nil {
		return err
	}
	if mounted(v.Path) {
		// Left over from a crash; it is unmarked and therefore unsafe.
		if err := unix.Unmount(v.Path, unix.MNT_DETACH); err != nil {
			return fmt.Errorf("remove stale view %s: %w", v.Path, err)
		}
	}
	staging, err := os.MkdirTemp("", "tether-view-")
	if err != nil {
		return err
	}
	defer os.Remove(staging)
	// Keep the staging mount private so it never propagates anywhere.
	if err := unix.Mount(v.Lower, staging, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s: %w", v.Lower, err)
	}
	_ = unix.Mount("", staging, "", unix.MS_PRIVATE, "")
	if err := unix.FanotifyMark(l.fd, unix.FAN_MARK_ADD|unix.FAN_MARK_MOUNT, eventMask, unix.AT_FDCWD, staging); err != nil {
		unix.Unmount(staging, unix.MNT_DETACH)
		return fmt.Errorf("fanotify_mark %s: %w", staging, err)
	}
	if err := unix.Mount(staging, v.Path, "", unix.MS_MOVE, ""); err != nil {
		unix.Unmount(staging, unix.MNT_DETACH)
		return fmt.Errorf("move view to %s: %w", v.Path, err)
	}
	l.mu.Lock()
	l.views = append(l.views, v)
	l.mu.Unlock()
	l.log.Info("Placeholder view mounted", "folder", v.Folder, "lower", v.Lower, "view", v.Path)
	return nil
}

// RemoveView detaches the view. Existing open file descriptors keep working.
func (l *Listener) RemoveView(path string) error {
	l.mu.Lock()
	for i, v := range l.views {
		if v.Path == path {
			l.views = append(l.views[:i], l.views[i+1:]...)
			break
		}
	}
	l.mu.Unlock()
	return unix.Unmount(path, unix.MNT_DETACH)
}

// Close unmounts all views and closes the group. Blocked accessors are
// released by the kernel.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		views := l.views
		l.views = nil
		l.mu.Unlock()
		for _, v := range views {
			_ = unix.Unmount(v.Path, unix.MNT_DETACH)
		}
		l.closeErr = unix.Close(l.fd)
	})
	return l.closeErr
}

// Forget removes the ignore mark for a file that just became a placeholder
// again (eviction), so accesses are intercepted once more. f may be opened
// through any path.
func (l *Listener) Forget(f *os.File) error {
	err := unix.FanotifyMark(l.fd, unix.FAN_MARK_REMOVE|unix.FAN_MARK_IGNORE, ignoreMask, int(f.Fd()), "")
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// Serve reads events until ctx is cancelled or the group is closed.
func (l *Listener) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(l.fd, buf)
		if err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("fanotify read: %w", err)
		}
		for off := 0; off+int(unsafe.Sizeof(unix.FanotifyEventMetadata{})) <= n; {
			md := *(*unix.FanotifyEventMetadata)(unsafe.Pointer(&buf[off]))
			if md.Event_len == 0 {
				break
			}
			off += int(md.Event_len)
			if md.Fd < 0 {
				continue // queue overflow; permission events are never dropped
			}
			go l.handle(ctx, md)
		}
	}
}

func (l *Listener) handle(ctx context.Context, md unix.FanotifyEventMetadata) {
	f := os.NewFile(uintptr(md.Fd), "event")
	defer f.Close()
	errno := l.decide(ctx, f, int(md.Pid))
	// A response the kernel rejects leaves the accessor blocked forever
	// (uninterruptibly), so fall back to ever simpler answers.
	for _, resp := range responses(errno) {
		var r [8]byte
		binary.LittleEndian.PutUint32(r[0:], uint32(md.Fd))
		binary.LittleEndian.PutUint32(r[4:], resp)
		_, err := unix.Write(l.fd, r[:])
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return // group is closing; the kernel releases waiters
		}
		l.log.Warn("fanotify response rejected", "response", resp, "error", err)
	}
}

// deniable lists the errnos the kernel accepts in FAN_DENY_ERRNO().
var deniable = map[unix.Errno]bool{
	unix.EPERM: true, unix.EIO: true, unix.EBUSY: true, unix.ETXTBSY: true,
	unix.EAGAIN: true, unix.ENOSPC: true, unix.EDQUOT: true,
}

func responses(errno unix.Errno) []uint32 {
	if errno == 0 {
		return []uint32{unix.FAN_ALLOW}
	}
	if !deniable[errno] {
		errno = unix.EIO
	}
	return []uint32{
		unix.FAN_DENY | (uint32(errno)&unix.FAN_ERRNO_MASK)<<unix.FAN_ERRNO_SHIFT,
		unix.FAN_DENY, // kernels without FAN_DENY_ERRNO (EPERM)
	}
}

func (l *Listener) decide(ctx context.Context, f *os.File, pid int) unix.Errno {
	fd := int(f.Fd())
	if !IsVirtual(f) {
		l.ignore(fd)
		return 0
	}
	path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err != nil {
		return unix.EIO
	}
	v, name := l.resolve(path)
	if v == nil {
		// A virtual file reached through a view we no longer own.
		return unix.EIO
	}
	if l.policy != nil {
		exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if errno := l.policy(pid, exe); errno != 0 {
			l.log.Debug("Hydration denied by policy", "folder", v.Folder, "file", name, "pid", pid, "exe", exe)
			return errno
		}
	}

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return unix.EIO
	}
	gen, _ := unix.IoctlGetInt(fd, fsIocGetVersion)
	key := fileKey{dev: st.Dev, ino: st.Ino, gen: gen}

	// Concurrent accesses to the same file share one hydration.
	fl := &flight{done: make(chan struct{})}
	if prev, loaded := l.flights.LoadOrStore(key, fl); loaded {
		fl = prev.(*flight)
		<-fl.done
	} else {
		fl.err = l.hydrate(ctx, v, name, f, &st)
		l.flights.Delete(key)
		close(fl.done)
	}
	if fl.err != nil {
		l.log.Warn("Hydration failed", "folder", v.Folder, "file", name, "error", fl.err)
		var errno unix.Errno
		if errors.As(fl.err, &errno) {
			return errno
		}
		return unix.EIO
	}
	l.ignore(fd)
	return 0
}

func (l *Listener) hydrate(ctx context.Context, v *View, name string, f *os.File, st *unix.Stat_t) error {
	if !IsVirtual(f) { // lost a race with another flight
		return nil
	}
	if err := l.handler.Hydrate(ctx, v.Folder, name, f); err != nil {
		return err
	}
	// Writing through the event fd bumped mtime; keep the placeholder's.
	ts := []unix.Timespec{st.Atim, st.Mtim}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", f.Fd()), ts, 0); err != nil {
		return err
	}
	if err := unix.Fremovexattr(int(f.Fd()), XattrState); err != nil && !errors.Is(err, unix.ENODATA) {
		return err
	}
	return nil
}

func (l *Listener) ignore(fd int) {
	_ = unix.FanotifyMark(l.fd, unix.FAN_MARK_ADD|unix.FAN_MARK_IGNORE_SURV|unix.FAN_MARK_EVICTABLE, ignoreMask, fd, "")
}

func (l *Listener) resolve(path string) (*View, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, v := range l.views {
		if rel, ok := strings.CutPrefix(path, v.Path+"/"); ok {
			return v, rel
		}
	}
	return nil, ""
}

// IsVirtual reports whether f carries the placeholder xattr.
func IsVirtual(f *os.File) bool {
	var b [16]byte
	n, err := unix.Fgetxattr(int(f.Fd()), XattrState, b[:])
	return err == nil && string(b[:n]) == StateVirtual
}

// MarkVirtual turns an open file into a placeholder of the given size:
// content is discarded (the blocks are freed) and the xattr set. The caller
// must ensure nobody else has the file open (see Lease). The file's mtime is
// preserved.
func MarkVirtual(f *os.File, size int64) error {
	fd := int(f.Fd())
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if err := unix.Fsetxattr(fd, XattrState, []byte(StateVirtual), 0); err != nil {
		return err
	}
	// Truncating to zero frees every block, including a partial last one
	// that a hole punch would keep.
	if err := unix.Ftruncate(fd, 0); err != nil {
		return err
	}
	if err := unix.Ftruncate(fd, size); err != nil {
		return err
	}
	ts := []unix.Timespec{st.Atim, st.Mtim}
	return unix.UtimesNanoAt(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), ts, 0)
}

// Lease takes a write lease on f, which succeeds only if no other open file
// descriptions refer to the file. While held, other opens block until
// Unlease. Needs CAP_LEASE or file ownership.
func Lease(f *os.File) error {
	_, err := unix.FcntlInt(f.Fd(), unix.F_SETLEASE, unix.F_WRLCK)
	if errors.Is(err, unix.EAGAIN) {
		return fmt.Errorf("file is in use: %w", syscall.EBUSY)
	}
	return err
}

func Unlease(f *os.File) error {
	_, err := unix.FcntlInt(f.Fd(), unix.F_SETLEASE, unix.F_UNLCK)
	return err
}

func mounted(path string) bool {
	var a, b unix.Stat_t
	if unix.Stat(path, &a) != nil || unix.Stat(filepath.Dir(path), &b) != nil {
		return false
	}
	if a.Dev != b.Dev {
		return true
	}
	data, _ := os.ReadFile("/proc/self/mountinfo")
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) > 4 && fields[4] == path {
			return true
		}
	}
	return false
}

// FS_IOC_GETVERSION: _IOR('v', 1, long)
const fsIocGetVersion = 0x80087601
