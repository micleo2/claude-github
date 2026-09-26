// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

// Package hsm implements on-demand hydration of placeholder files using
// fanotify pre-content events (Linux >= 6.14).
//
// A placeholder is a sparse file of the right size and mtime whose xattr
// user.tether.state is "virtual". Each placeholder's inode carries a fanotify
// mark, so opening or accessing it through any path blocks the caller until
// the Handler has written the full content through the event fd. That
// includes paths in other mount namespaces (containers, flatpak), which a
// mount mark would miss: see docs/research/placeholder-access-paths.md.
// Marks are added before a file becomes a placeholder and removed once it
// is hydrated, so other files never generate events.
//
// A folder's real directory (the "lower" path) is used by the sync engine,
// whose own accesses are let through without hydration. Users work in a bind
// mount of it (the "view"). The listener writes downloaded content through a
// private, detached clone of lower whose accesses raise no events: the event
// fds are read-only, so that accesses through read-only mounts work too.
package hsm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// XattrState holds "virtual" on placeholders. It is a fast-path hint;
	// the sync database is authoritative.
	XattrState   = "user.tether.state"
	StateVirtual = "virtual"
	// XattrOrigin and XattrBlocksHash record which file (name, block list
	// hash) a placeholder stands for, so that a moved placeholder can be
	// identified without reading it.
	XattrOrigin     = "user.tether.origin"
	XattrBlocksHash = "user.tether.bh"
	// XattrHydrating is set while content is being written into a
	// placeholder and holds its modification time from before (writing
	// bumps it). If it is still there when nobody is hydrating, a hydration
	// failed or was interrupted: see Discard.
	XattrHydrating = "user.tether.hydrating"
	// XattrPrefix covers all of the above. These attributes are local
	// bookkeeping and must never be synced.
	XattrPrefix = "user.tether."

	eventMask = unix.FAN_OPEN_PERM | unix.FAN_PRE_ACCESS
)

// Handler supplies file content.
type Handler interface {
	// Hydrate writes the complete content of the placeholder at name
	// (relative to the view's folder) into f, then calls Finish(f), and
	// returns nil. Otherwise it returns an error whose errno (if any) is
	// reported to the blocked application.
	Hydrate(ctx context.Context, folder, name string, f *os.File) error
}

// Policy decides whether a process may trigger hydration of a file in the
// given folder. Returning a non-zero errno denies the access with that
// errno.
type Policy func(folder string, pid int, exe string) unix.Errno

// View is a bind mount of a folder.
type View struct {
	Folder string // folder ID
	Lower  string // real directory used by the sync engine
	Path   string // user-visible mount point

	// A detached clone of Lower with an ignore mark: opening files through
	// it never raises events. Owned by the Listener.
	private int
}

type Listener struct {
	fd      int
	handler Handler
	policy  Policy
	log     *slog.Logger
	// self is the process whose accesses never hydrate: the sync engine
	// reads and writes placeholders as ordinary files.
	self atomic.Int32

	mu    sync.Mutex
	views []*View

	flights sync.Map // fileKey -> *flight

	closeOnce sync.Once
	// fdMu keeps responses from being written after Close: the fd number
	// may by then name another file, or another listener's event.
	fdMu     sync.RWMutex
	closed   bool
	wake     int // eventfd that interrupts Serve's poll on Close
	closeErr error
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

// GroupEnv names the environment variable through which the monitor
// process passes the fanotify group (see NewGroup) to the sync process.
const GroupEnv = "TETHER_HSM_GROUP_FD"

// NewGroup creates the fanotify group. The monitor process creates it and
// passes it to every sync process it starts, so that the group, and with it
// every placeholder's mark, outlives a crashed sync process: accesses in the
// meantime wait in the queue for the next one instead of reading zeros.
func NewGroup() (int, error) {
	// Event fds are read-only: a read-write one cannot be opened for an
	// access through a read-only mount (docker -v ...:ro). FD_ERROR reports
	// such failures in the event instead of failing read().
	// Non-blocking, so that a listener being closed never sits in read()
	// and takes an event that its successor should get (see Serve).
	fd, err := unix.FanotifyInit(unix.FAN_CLASS_PRE_CONTENT|unix.FAN_CLOEXEC|unix.FAN_NONBLOCK|
		unix.FAN_UNLIMITED_QUEUE|unix.FAN_UNLIMITED_MARKS|unix.FAN_REPORT_FD_ERROR, unix.O_RDONLY|unix.O_LARGEFILE)
	if err != nil {
		return -1, fmt.Errorf("fanotify_init: %w", err)
	}
	return fd, nil
}

// New starts a listener on the group inherited from the monitor process
// (GroupEnv), or on a new group.
func New(handler Handler, policy Policy, log *slog.Logger) (*Listener, error) {
	if log == nil {
		log = slog.Default()
	}
	fd, inherited, err := group()
	if err != nil {
		return nil, err
	}
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("eventfd: %w", err)
	}
	l := &Listener{fd: fd, wake: wake, handler: handler, policy: policy, log: log}
	l.self.Store(int32(os.Getpid()))
	if inherited {
		l.clearStale()
	}
	return l, nil
}

func group() (fd int, inherited bool, err error) {
	s := os.Getenv(GroupEnv)
	if s == "" {
		fd, err := NewGroup()
		return fd, false, err
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1, false, fmt.Errorf("%s=%q: %w", GroupEnv, s, err)
	}
	// A duplicate, so that closing the listener leaves the inherited group
	// alone.
	fd, err = unix.FcntlInt(uintptr(n), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return -1, false, fmt.Errorf("inherited fanotify group (fd %d): %w", n, err)
	}
	return fd, true, nil
}

// clearStale answers the permission events that an earlier sync process
// read but did not answer before it died. Their accessors fail with EIO
// (the download they waited for was lost); events that were queued but
// never read are served normally. Responses are matched by event fd number
// alone, so this must happen before this process reads any event: a new
// event could otherwise share a stale one's number, and an answer meant for
// it would release the stale one instead.
func (l *Listener) clearStale() {
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rl); err != nil || rl.Cur > 1<<22 {
		rl.Cur = 1 << 22
	}
	var r [8]byte
	binary.LittleEndian.PutUint32(r[4:], responses(unix.EIO)[0])
	n := 0
	start := time.Now()
	for fd := uint64(0); fd < rl.Cur; fd++ {
		binary.LittleEndian.PutUint32(r[0:], uint32(fd))
		if _, err := unix.Write(l.fd, r[:]); err == nil {
			n++
		}
	}
	log := l.log.Debug
	if n > 0 {
		log = l.log.Warn
	}
	log("Resumed the on-demand group from the previous process; interrupted downloads fail with EIO",
		"interrupted", n, "checked", rl.Cur, "duration", time.Since(start).Round(time.Millisecond))
}

// AddView starts serving a folder: it marks every placeholder under
// v.Lower (marks do not outlive the listener), then mounts v.Lower at v.Path.
// Placeholders are protected by their own marks, not by the view.
//
// The view is two mounts. The bottom one is path bound onto itself and made
// private, and the top one is a private clone of lower. Neither propagates, so
// when the monitor unmounts a crashed daemon's view nothing is left behind in
// other mount namespaces that existed before it. Attaching a detached clone
// (rather than MS_MOVE) works when the parent mounts are shared, as they are
// on any systemd host.
func (l *Listener) AddView(v *View) error {
	if err := l.probe(v.Lower); err != nil {
		return err
	}
	priv, err := unix.OpenTree(unix.AT_FDCWD, v.Lower, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return fmt.Errorf("clone %s: %w", v.Lower, err)
	}
	if err := unix.FanotifyMark(l.fd, unix.FAN_MARK_ADD|unix.FAN_MARK_MOUNT|unix.FAN_MARK_IGNORE_SURV, eventMask, priv, "."); err != nil {
		unix.Close(priv)
		return fmt.Errorf("ignore mark on private mount of %s: %w", v.Lower, err)
	}
	v.private = priv
	// Registered before marking, so that events can be served at once.
	l.mu.Lock()
	l.views = append(l.views, v)
	l.mu.Unlock()
	if err := l.mountView(v); err != nil {
		l.forget(v)
		return err
	}
	return nil
}

func (l *Listener) forget(v *View) {
	l.mu.Lock()
	l.views = slices.DeleteFunc(l.views, func(x *View) bool { return x == v })
	l.mu.Unlock()
	unix.Close(v.private)
}

func (l *Listener) mountView(v *View) error {
	n, err := l.MarkTree(v.Lower)
	if err != nil {
		return err
	}
	l.log.Info("Marked placeholders for on-demand download", "folder", v.Folder, "count", n)
	if err := os.MkdirAll(v.Path, 0o755); err != nil {
		return err
	}
	// Anything mounted here is left over from a crash.
	if err := UnmountView(v.Path); err != nil {
		return fmt.Errorf("remove stale view %s: %w", v.Path, err)
	}
	// Mounting over existing files would hide them for as long as tether
	// runs.
	if ents, err := os.ReadDir(v.Path); err != nil {
		return err
	} else if len(ents) > 0 {
		return fmt.Errorf("view path %s is not empty", v.Path)
	}
	if err := unix.Mount(v.Path, v.Path, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s: %w", v.Path, err)
	}
	fail := func(err error) error {
		_ = UnmountView(v.Path)
		return err
	}
	if err := unix.Mount("", v.Path, "", unix.MS_PRIVATE, ""); err != nil {
		return fail(fmt.Errorf("make %s private: %w", v.Path, err))
	}
	tree, err := unix.OpenTree(unix.AT_FDCWD, v.Lower, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return fail(fmt.Errorf("clone %s: %w", v.Lower, err))
	}
	defer unix.Close(tree)
	// A clone keeps the source's propagation and would join the peer group
	// of lower's mount.
	if err := unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH, &unix.MountAttr{Propagation: unix.MS_PRIVATE}); err != nil {
		return fail(fmt.Errorf("make view of %s private: %w", v.Lower, err))
	}
	if err := unix.MoveMount(tree, "", unix.AT_FDCWD, v.Path, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fail(fmt.Errorf("attach view at %s: %w", v.Path, err))
	}
	l.log.Info("On-demand view mounted", "folder", v.Folder, "lower", v.Lower, "view", v.Path)
	return nil
}

// RemoveView detaches the view. Existing open file descriptors keep working.
func (l *Listener) RemoveView(path string) error {
	l.mu.Lock()
	for i, v := range l.views {
		if v.Path == path {
			l.views = append(l.views[:i], l.views[i+1:]...)
			unix.Close(v.private)
			break
		}
	}
	l.mu.Unlock()
	return UnmountView(path)
}

// UnmountView detaches every mount stacked at path (a view is two; see
// AddView). Nothing mounted there is not an error.
func UnmountView(path string) error {
	for i := 0; i < 8 && mounted(path); i++ {
		if err := unix.Unmount(path, unix.MNT_DETACH); err != nil {
			if err == unix.EINVAL || err == unix.ENOENT {
				return nil
			}
			return err
		}
	}
	return nil
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
			_ = UnmountView(v.Path)
			unix.Close(v.private)
		}
		var one [8]byte
		binary.LittleEndian.PutUint64(one[:], 1)
		_, _ = unix.Write(l.wake, one[:])
		l.fdMu.Lock()
		l.closed = true
		l.closeErr = unix.Close(l.fd)
		unix.Close(l.wake)
		l.fdMu.Unlock()
	})
	return l.closeErr
}

// MakePlaceholder turns f into a marked placeholder (see MarkVirtual), so
// that accesses through any path wait for hydration. There is never a
// moment in which f is a placeholder without a mark. Nobody else may have f
// open: it is either a fresh temporary file or held under a Lease.
func (l *Listener) MakePlaceholder(f *os.File, size int64, origin string, blocksHash []byte) error {
	fd := int(f.Fd())
	// Opens first. Access events are added only after the truncation in
	// MarkVirtual, which would otherwise raise one: delivering it opens the
	// file, which waits for our own lease to break (45 s).
	if err := l.markMask(fd, unix.FAN_OPEN_PERM); err != nil {
		return err
	}
	if err := MarkVirtual(f, size, origin, blocksHash); err != nil {
		return err
	}
	return l.markMask(fd, unix.FAN_PRE_ACCESS)
}

// Evict turns the local file name in folder into a placeholder, if check
// approves of its content. check runs while nobody else can open the file.
//
// The file is marked before anyone is locked out: the kernel decides at
// open time, before waiting for our lease, whether a file raises
// pre-content events. An open that raced with a later mark would read the
// placeholder's zeros. Our own accesses go through the private mount,
// which raises no events: the event fd for our truncation would have to
// wait for our own lease.
func (l *Listener) Evict(folder, name string, size int64, origin string, blocksHash []byte, check func(*os.File) error) error {
	l.mu.Lock()
	var v *View
	for _, x := range l.views {
		if x.Folder == folder {
			v = x
		}
	}
	if v == nil {
		l.mu.Unlock()
		return fmt.Errorf("no view for folder %s", folder)
	}
	f, restore, err := OpenForWrite(v.private, name)
	l.mu.Unlock()
	if err != nil {
		return err
	}
	defer f.Close()
	defer restore()
	fd := int(f.Fd())
	if err := l.markMask(fd, eventMask); err != nil {
		return err
	}
	evicted := false
	defer func() {
		if !evicted {
			l.unmark(fd)
		}
	}()
	// While the lease is held nobody else can open the file, so nothing can
	// change it between the check and the eviction.
	if err := Lease(f); err != nil {
		return err
	}
	defer Unlease(f) //nolint:errcheck
	if err := check(f); err != nil {
		return err
	}
	if err := MarkVirtual(f, size, origin, blocksHash); err != nil {
		evicted = IsVirtual(f) // keep the mark on anything that says it is a placeholder
		return err
	}
	evicted = true
	return nil
}

func (l *Listener) markMask(fd int, mask uint64) error {
	if err := unix.FanotifyMark(l.fd, unix.FAN_MARK_ADD, mask, fd, ""); err != nil {
		return fmt.Errorf("fanotify_mark: %w", err)
	}
	return nil
}

// Unmark removes f's mark once it is no longer a placeholder.
func (l *Listener) Unmark(f *os.File) {
	l.unmark(int(f.Fd()))
}

func (l *Listener) unmark(fd int) {
	_ = unix.FanotifyMark(l.fd, unix.FAN_MARK_REMOVE, eventMask, fd, "")
}

// probe checks that lower's filesystem supports pre-content marks, using an
// unnamed temporary file.
func (l *Listener) probe(lower string) error {
	probe, err := unix.Open(lower, unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create probe file in %s: %w", lower, err)
	}
	err = l.markMask(probe, eventMask)
	l.unmark(probe)
	unix.Close(probe)
	if err != nil {
		return fmt.Errorf("%s does not support on-demand files (ext4, xfs or btrfs needed): %w", lower, err)
	}
	return nil
}

// MarkTree marks every placeholder under lower and returns how many there
// were. AddView does this before mounting the view.
func (l *Listener) MarkTree(lower string) (int, error) {
	n := 0
	err := filepath.WalkDir(lower, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil // unreadable entries are the scanner's business
		}
		var b [16]byte
		sz, err := unix.Lgetxattr(path, XattrState, b[:])
		if err != nil || string(b[:sz]) != StateVirtual {
			return nil
		}
		if err := unix.FanotifyMark(l.fd, unix.FAN_MARK_ADD|unix.FAN_MARK_DONT_FOLLOW, eventMask, unix.AT_FDCWD, path); err != nil {
			return fmt.Errorf("mark %s: %w", path, err)
		}
		n++
		return nil
	})
	return n, err
}

// Serve reads events until ctx is cancelled or the group is closed.
func (l *Listener) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	buf := make([]byte, 64<<10)
	for {
		// Wait without holding the lock, then read (non-blocking) only if
		// not closed: an event read here is ours to answer, so a closing
		// listener must not take one.
		fds := []unix.PollFd{{Fd: int32(l.fd), Events: unix.POLLIN}, {Fd: int32(l.wake), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, -1); err != nil && !errors.Is(err, unix.EINTR) {
			l.log.Warn("fanotify poll failed", "error", err)
			time.Sleep(10 * time.Millisecond)
		}
		l.fdMu.RLock()
		if l.closed {
			l.fdMu.RUnlock()
			return ctx.Err()
		}
		n, err := unix.Read(l.fd, buf)
		l.fdMu.RUnlock()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Never stop serving while the marks exist: every access to a
			// placeholder would wait forever.
			if !errors.Is(err, unix.EINTR) && !errors.Is(err, unix.EAGAIN) {
				l.log.Warn("fanotify read failed", "error", err)
				time.Sleep(10 * time.Millisecond)
			}
			continue
		}
		for off := 0; off+int(unsafe.Sizeof(unix.FanotifyEventMetadata{})) <= n; {
			md := *(*unix.FanotifyEventMetadata)(unsafe.Pointer(&buf[off]))
			if md.Event_len == 0 {
				break
			}
			off += int(md.Event_len)
			if md.Fd < 0 {
				// Queue overflow, or (FD_ERROR) the kernel could not open
				// the file for us; it has already denied the access.
				if md.Fd != unix.FAN_NOFD {
					l.log.Warn("Access to a placeholder denied: cannot open it", "pid", md.Pid, "error", unix.Errno(-md.Fd))
				}
				continue
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
		l.fdMu.RLock()
		if l.closed {
			l.fdMu.RUnlock()
			return // a shared group keeps it pending for the next listener
		}
		_, err := unix.Write(l.fd, r[:])
		l.fdMu.RUnlock()
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
	if int32(pid) == l.self.Load() {
		return 0
	}
	if !IsVirtual(f) {
		l.unmark(fd)
		return 0
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return unix.EIO
	}
	v, name := l.resolve(f, &st)
	if v == nil {
		l.log.Warn("Placeholder opened through a path that cannot be matched to a folder; denied", "pid", pid, "path", fdPath(fd))
		return unix.EIO
	}
	if l.policy != nil {
		exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if errno := l.policy(v.Folder, pid, exe); errno != 0 {
			l.log.Debug("Hydration denied by policy", "folder", v.Folder, "file", name, "pid", pid, "exe", exe)
			return errno
		}
	}

	gen, _ := unix.IoctlGetInt(fd, fsIocGetVersion) //nolint:gosec
	key := fileKey{dev: st.Dev, ino: st.Ino, gen: gen}

	// Concurrent accesses to the same file share one hydration.
	fl := &flight{done: make(chan struct{})}
	if prev, loaded := l.flights.LoadOrStore(key, fl); loaded {
		fl = prev.(*flight)
		<-fl.done
	} else {
		fl.err = l.hydrateVia(ctx, v, name, &st)
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
	l.unmark(fd)
	return 0
}

// hydrateVia opens name writable through v's private mount (the event fd is
// read-only) and hydrates it, checking that it is the inode of the event.
func (l *Listener) hydrateVia(ctx context.Context, v *View, name string, st *unix.Stat_t) error {
	f, restore, err := OpenForWrite(v.private, name)
	if err != nil {
		return err
	}
	defer f.Close()
	defer restore()
	var wst unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &wst); err != nil {
		return err
	}
	if wst.Dev != st.Dev || wst.Ino != st.Ino {
		return fmt.Errorf("%s was replaced during hydration: %w", name, unix.EIO)
	}
	return l.hydrate(ctx, v, name, f)
}

func (l *Listener) hydrate(ctx context.Context, v *View, name string, f *os.File) error {
	if !IsVirtual(f) { // lost a race with another flight
		return nil
	}
	if err := l.handler.Hydrate(ctx, v.Folder, name, f); err != nil {
		return err
	}
	if IsVirtual(f) {
		return fmt.Errorf("handler did not finish hydration: %w", unix.EIO)
	}
	return nil
}

func fdPath(fd int) string {
	p, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	return p
}

// resolve finds the folder and name of the placeholder open as f (stat st).
// The event's path is as the accessing process sees it, which for a process
// in another mount namespace means nothing here, so paths are only hints:
// each candidate name must lead to the same inode. Candidates are the path
// under a view or lower directory, the name the placeholder was created
// under, and every trailing part of the path (a container's bind mount of
// the folder).
func (l *Listener) resolve(f *os.File, st *unix.Stat_t) (*View, string) {
	path := fdPath(int(f.Fd()))
	origin, _, _ := ReadPlaceholderFile(f)
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	l.mu.Lock()
	views := slices.Clone(l.views)
	l.mu.Unlock()
	for _, v := range views {
		var cands []string
		for _, root := range []string{v.Path, v.Lower} {
			if rel, ok := strings.CutPrefix(path, root+"/"); ok {
				cands = append(cands, rel)
			}
		}
		cands = append(cands, origin)
		for i := range parts {
			cands = append(cands, strings.Join(parts[i:], "/"))
		}
		for _, name := range cands {
			if name == "" || !filepath.IsLocal(name) {
				continue
			}
			var cst unix.Stat_t
			if unix.Lstat(filepath.Join(v.Lower, name), &cst) == nil && cst.Dev == st.Dev && cst.Ino == st.Ino {
				return v, name
			}
		}
	}
	return nil, ""
}

// OpenForWrite opens name (relative to dirfd, not following a final
// symlink) to write its content in place. A file its owner may not write
// (git makes objects 0444) is made writable until restore is called; a
// process with CAP_DAC_OVERRIDE opens it directly. Call restore before
// closing the file.
func OpenForWrite(dirfd int, name string) (f *os.File, restore func(), err error) {
	const flags = unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(dirfd, name, flags, 0)
	if err == nil {
		return os.NewFile(uintptr(fd), name), func() {}, nil
	}
	if !errors.Is(err, unix.EACCES) {
		return nil, nil, err
	}
	denied := err
	// An O_PATH open raises no fanotify events and needs no permission.
	pfd, err := unix.Openat(dirfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, denied
	}
	defer unix.Close(pfd)
	var st unix.Stat_t
	if unix.Fstat(pfd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG ||
		int(st.Uid) != os.Geteuid() || st.Mode&0o200 != 0 {
		return nil, nil, denied
	}
	mode := st.Mode & 0o7777
	proc := fmt.Sprintf("/proc/self/fd/%d", pfd)
	if err := unix.Fchmodat(unix.AT_FDCWD, proc, mode|0o200, 0); err != nil {
		return nil, nil, denied
	}
	fd, err = unix.Openat(dirfd, name, flags, 0)
	var wst unix.Stat_t
	if err == nil && (unix.Fstat(fd, &wst) != nil || wst.Dev != st.Dev || wst.Ino != st.Ino) {
		unix.Close(fd)
		err = fmt.Errorf("%s was replaced while opening it: %w", name, unix.EIO)
	}
	if err != nil {
		_ = unix.Fchmodat(unix.AT_FDCWD, proc, mode, 0)
		return nil, nil, err
	}
	f = os.NewFile(uintptr(fd), name)
	return f, func() { _ = unix.Fchmod(int(f.Fd()), mode) }, nil
}

// OpenPathForWrite is OpenForWrite for a path.
func OpenPathForWrite(path string) (*os.File, func(), error) {
	return OpenForWrite(unix.AT_FDCWD, path)
}

// IsVirtual reports whether f carries the placeholder xattr.
func IsVirtual(f *os.File) bool {
	var b [16]byte
	n, err := unix.Fgetxattr(int(f.Fd()), XattrState, b[:])
	return err == nil && string(b[:n]) == StateVirtual
}

// ReadPlaceholder inspects the file at path (not following symlinks). It
// reports whether it is a placeholder and, if recorded, which file it
// stands for.
func ReadPlaceholder(path string) (origin string, blocksHash []byte, isPlaceholder bool) {
	var b [16]byte
	n, err := unix.Lgetxattr(path, XattrState, b[:])
	if err != nil || string(b[:n]) != StateVirtual {
		return "", nil, false
	}
	buf := make([]byte, 4096)
	if n, err := unix.Lgetxattr(path, XattrOrigin, buf); err == nil {
		origin = string(buf[:n])
	}
	if n, err := unix.Lgetxattr(path, XattrBlocksHash, buf); err == nil {
		blocksHash = append([]byte(nil), buf[:n]...)
	}
	return origin, blocksHash, true
}

// ReadPlaceholderFile is ReadPlaceholder for an open file.
func ReadPlaceholderFile(f *os.File) (origin string, blocksHash []byte, isPlaceholder bool) {
	fd := int(f.Fd())
	var b [16]byte
	n, err := unix.Fgetxattr(fd, XattrState, b[:])
	if err != nil || string(b[:n]) != StateVirtual {
		return "", nil, false
	}
	buf := make([]byte, 4096)
	if n, err := unix.Fgetxattr(fd, XattrOrigin, buf); err == nil {
		origin = string(buf[:n])
	}
	if n, err := unix.Fgetxattr(fd, XattrBlocksHash, buf); err == nil {
		blocksHash = append([]byte(nil), buf[:n]...)
	}
	return origin, blocksHash, true
}

// BeginHydration records f's modification time in XattrHydrating before
// content is written into it, and returns it. A marker left by an earlier
// attempt is kept: it holds the time from before that attempt, which is
// returned instead.
func BeginHydration(f *os.File) (mtime time.Time, err error) {
	fd := int(f.Fd())
	var b [8]byte
	if n, err := unix.Fgetxattr(fd, XattrHydrating, b[:]); err == nil && n == len(b) {
		return time.Unix(0, int64(binary.LittleEndian.Uint64(b[:]))), nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return time.Time{}, err
	}
	binary.LittleEndian.PutUint64(b[:], uint64(st.Mtim.Nano()))
	if err := unix.Fsetxattr(fd, XattrHydrating, b[:], 0); err != nil {
		return time.Time{}, err
	}
	return time.Unix(st.Mtim.Unix()), nil
}

// Interrupted reports whether the placeholder at path holds content from a
// hydration that failed or was interrupted (see XattrHydrating).
func Interrupted(path string) bool {
	_, err := unix.Lgetxattr(path, XattrHydrating, nil)
	return err == nil
}

// Discard undoes a failed hydration of the placeholder f: the partial
// content is freed and the modification time restored, so that the file is
// exactly the placeholder it was. Otherwise the scanner would take the
// bumped modification time for a local change and announce a new version.
// Nobody else may be hydrating f.
func Discard(f *os.File) error {
	fd := int(f.Fd())
	var b [8]byte
	n, err := unix.Fgetxattr(fd, XattrHydrating, b[:])
	if errors.Is(err, unix.ENODATA) {
		return nil // nothing was written
	} else if err != nil {
		return err
	}
	if n != len(b) {
		return fmt.Errorf("corrupt %s", XattrHydrating)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if err := unix.Ftruncate(fd, 0); err != nil {
		return err
	}
	if err := unix.Ftruncate(fd, st.Size); err != nil {
		return err
	}
	ts := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, unix.NsecToTimespec(int64(binary.LittleEndian.Uint64(b[:])))}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), ts, 0); err != nil {
		return err
	}
	return unix.Fremovexattr(fd, XattrHydrating)
}

// Retarget makes the placeholder f stand for other content: a different
// size, identity and modification time. Nobody else may be hydrating f.
func Retarget(f *os.File, size int64, origin string, blocksHash []byte, mtime time.Time) error {
	if err := Discard(f); err != nil {
		return err
	}
	if err := MarkVirtual(f, size, origin, blocksHash); err != nil {
		return err
	}
	ts := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, unix.NsecToTimespec(mtime.UnixNano())}
	return unix.UtimesNanoAt(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", int(f.Fd())), ts, 0)
}

// Ctime returns the status change time of the file at path.
func Ctime(path string) (time.Time, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return time.Time{}, err
	}
	return time.Unix(st.Ctim.Unix()), nil
}

// Unlinked reports whether f has no name left.
func Unlinked(f *os.File) bool {
	var st unix.Stat_t
	return unix.Fstat(int(f.Fd()), &st) == nil && st.Nlink == 0
}

// Finish completes a hydration: the placeholder attributes are removed, the
// modification time is reset to mtime (writing the content bumped it) and
// the access time set to atime.
func Finish(f *os.File, mtime, atime time.Time) error {
	fd := int(f.Fd())
	ts := []unix.Timespec{unix.NsecToTimespec(atime.UnixNano()), unix.NsecToTimespec(mtime.UnixNano())}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), ts, 0); err != nil {
		return err
	}
	// The hydration marker goes last: until the state is gone, it tells
	// the scanner that the modification time is not to be trusted.
	for _, name := range []string{XattrOrigin, XattrBlocksHash, XattrState, XattrHydrating} {
		if err := unix.Fremovexattr(fd, name); err != nil && !errors.Is(err, unix.ENODATA) {
			return err
		}
	}
	return nil
}

// MarkVirtual turns an open file into a placeholder of the given size:
// content is discarded (the blocks are freed) and the xattrs set. origin and
// blocksHash identify the content it stands for. The caller must ensure
// nobody else has the file open (see Lease). The file's mtime is preserved.
func MarkVirtual(f *os.File, size int64, origin string, blocksHash []byte) error {
	fd := int(f.Fd())
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if err := unix.Fsetxattr(fd, XattrState, []byte(StateVirtual), 0); err != nil {
		return err
	}
	if err := unix.Fsetxattr(fd, XattrOrigin, []byte(origin), 0); err != nil {
		return err
	}
	if err := unix.Fsetxattr(fd, XattrBlocksHash, blocksHash, 0); err != nil {
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
