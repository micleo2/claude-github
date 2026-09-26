// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

package hsm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

type mapHandler struct {
	mu    sync.Mutex
	data  map[string][]byte
	calls atomic.Int32
	delay time.Duration
}

func (h *mapHandler) Hydrate(_ context.Context, folder, name string, f *os.File) error {
	h.calls.Add(1)
	time.Sleep(h.delay)
	h.mu.Lock()
	d, ok := h.data[folder+"/"+name]
	h.mu.Unlock()
	if !ok {
		return unix.ENOENT
	}
	if _, err := f.WriteAt(d, 0); err != nil {
		return err
	}
	return Finish(f, time.Unix(1700000000, 0), time.Now())
}

func setup(t *testing.T, h Handler, p Policy) (lower, view string, l *Listener) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if err := Supported(); err != nil {
		t.Skip(err)
	}
	base := t.TempDir()
	lower = filepath.Join(base, "lower")
	view = filepath.Join(base, "view")
	os.MkdirAll(lower, 0o755)
	l, err := New(h, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The tests access placeholders from this process, which is otherwise
	// exempt as the sync engine.
	l.self.Store(-1)
	if err := l.AddView(&View{Folder: "f", Lower: lower, Path: view}); err != nil {
		t.Skipf("cannot create view (filesystem without HSM support?): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go l.Serve(ctx)
	t.Cleanup(func() { cancel(); l.Close() })
	return lower, view, l
}

// placeholder creates the placeholder lower/name; with l == nil it is left
// unmarked, as after a restart.
func placeholder(t *testing.T, l *Listener, lower, name string, size int64) {
	t.Helper()
	f, err := os.Create(filepath.Join(lower, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if l != nil {
		err = l.MakePlaceholder(f, size, name, []byte("bh"))
	} else {
		err = MarkVirtual(f, size, name, []byte("bh"))
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestHydrateOnOpen(t *testing.T) {
	content := bytes.Repeat([]byte("tether!"), 100000)
	h := &mapHandler{data: map[string][]byte{"f/dir/a": content}}
	lower, view, l := setup(t, h, nil)
	os.Mkdir(filepath.Join(lower, "dir"), 0o755)
	placeholder(t, l, lower, "dir/a", int64(len(content)))

	got, err := os.ReadFile(filepath.Join(view, "dir/a"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("content mismatch")
	}
	f, _ := os.Open(filepath.Join(lower, "dir/a"))
	defer f.Close()
	if IsVirtual(f) {
		t.Fatal("still virtual after hydration")
	}
	if _, _, ph := ReadPlaceholder(filepath.Join(lower, "dir/a")); ph {
		t.Fatal("placeholder xattrs left behind")
	}
	if st, _ := f.Stat(); st.ModTime().Unix() != 1700000000 {
		t.Fatal("mtime not restored")
	}
	// Second read is served from disk.
	os.ReadFile(filepath.Join(view, "dir/a"))
	if n := h.calls.Load(); n != 1 {
		t.Fatalf("handler called %d times", n)
	}
}

func TestReadPlaceholder(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root for user xattrs on some filesystems")
	}
	p := filepath.Join(t.TempDir(), "ph")
	f, _ := os.Create(p)
	defer f.Close()
	if err := MarkVirtual(f, 123, "orig/name", []byte{1, 2, 3}); err != nil {
		t.Skip(err)
	}
	origin, bh, ok := ReadPlaceholder(p)
	if !ok || origin != "orig/name" || !bytes.Equal(bh, []byte{1, 2, 3}) {
		t.Fatalf("got %q %v %v", origin, bh, ok)
	}
	if st, _ := f.Stat(); st.Size() != 123 {
		t.Fatal("size")
	}
}

func TestSparseCopierSeesContent(t *testing.T) {
	content := bytes.Repeat([]byte{0xAB}, 3<<20)
	h := &mapHandler{data: map[string][]byte{"f/big": content}}
	lower, view, l := setup(t, h, nil)
	placeholder(t, l, lower, "big", int64(len(content)))
	dst := filepath.Join(t.TempDir(), "copy")
	if out, err := exec.Command("cp", filepath.Join(view, "big"), dst).CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, content) {
		t.Fatal("cp produced wrong content")
	}
}

func TestConcurrentOpenersShareOneHydration(t *testing.T) {
	content := []byte("hello")
	h := &mapHandler{data: map[string][]byte{"f/x": content}, delay: 200 * time.Millisecond}
	lower, view, l := setup(t, h, nil)
	placeholder(t, l, lower, "x", int64(len(content)))
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := os.ReadFile(filepath.Join(view, "x"))
			if err != nil || !bytes.Equal(got, content) {
				t.Errorf("got %q, %v", got, err)
			}
		}()
	}
	wg.Wait()
	if n := h.calls.Load(); n != 1 {
		t.Fatalf("handler called %d times", n)
	}
}

func TestErrnoPropagates(t *testing.T) {
	h := &mapHandler{data: map[string][]byte{}}
	lower, view, l := setup(t, h, nil)
	placeholder(t, l, lower, "missing", 10)
	_, err := os.ReadFile(filepath.Join(view, "missing"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestPolicyDenies(t *testing.T) {
	h := &mapHandler{data: map[string][]byte{"f/x": []byte("hi")}}
	lower, view, l := setup(t, h, func(folder string, pid int, exe string) unix.Errno {
		if filepath.Base(exe) == "denied-cat" {
			return unix.EAGAIN
		}
		return 0
	})
	placeholder(t, l, lower, "x", 2)
	// Run cat from a copy with a distinctive name. Keep argv[0] "cat" so a
	// multi-call binary such as busybox still acts as cat.
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Skip(err)
	}
	data, _ := os.ReadFile(catPath)
	denied := filepath.Join(t.TempDir(), "denied-cat")
	if err := os.WriteFile(denied, data, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := &exec.Cmd{Path: denied, Args: []string{"cat", filepath.Join(view, "x")}}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("cat should have been denied, got %q", out)
	}
	if h.calls.Load() != 0 {
		t.Fatal("denied access still hydrated")
	}
}

func TestEvictAndRehydrate(t *testing.T) {
	content := []byte("round trip")
	h := &mapHandler{data: map[string][]byte{"f/r": content}}
	lower, view, l := setup(t, h, nil)
	placeholder(t, l, lower, "r", int64(len(content)))
	if got, _ := os.ReadFile(filepath.Join(view, "r")); !bytes.Equal(got, content) {
		t.Fatal("first hydration")
	}

	start := time.Now()
	if err := l.Evict("f", "r", int64(len(content)), "r", []byte("bh"), func(*os.File) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("eviction under a lease took %v (event raised against our own lease?)", d)
	}

	var st unix.Stat_t
	unix.Stat(filepath.Join(lower, "r"), &st)
	// Data blocks are gone; at most one filesystem block may hold the
	// placeholder xattrs when they do not fit inside the inode.
	if st.Blocks*512 > 4096 {
		t.Fatalf("evicted file still has %d blocks", st.Blocks)
	}
	if got, _ := os.ReadFile(filepath.Join(view, "r")); !bytes.Equal(got, content) {
		t.Fatalf("rehydration got %q", got)
	}
	if n := h.calls.Load(); n != 2 {
		t.Fatalf("handler called %d times", n)
	}
}

func TestLeaseFailsWhileOpen(t *testing.T) {
	h := &mapHandler{data: map[string][]byte{"f/o": []byte("x")}}
	lower, view, _ := setup(t, h, nil)
	os.WriteFile(filepath.Join(lower, "o"), []byte("x"), 0o644)
	holder, err := os.Open(filepath.Join(view, "o"))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	f, _ := os.OpenFile(filepath.Join(lower, "o"), os.O_RDWR, 0)
	defer f.Close()
	if err := Lease(f); err == nil {
		t.Fatal("lease should fail while another process has the file open")
	}
}

func TestViewRemovedOnClose(t *testing.T) {
	h := &mapHandler{}
	lower, view, l := setup(t, h, nil)
	os.WriteFile(filepath.Join(lower, "plain"), []byte("x"), 0o644)
	if _, err := os.Stat(filepath.Join(view, "plain")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if _, err := os.Stat(filepath.Join(view, "plain")); err == nil {
		t.Fatal("view still reachable after Close")
	}
}

func TestHydrateThroughLowerPath(t *testing.T) {
	content := []byte("via lower")
	h := &mapHandler{data: map[string][]byte{"f/x": content}}
	lower, _, l := setup(t, h, nil)
	placeholder(t, l, lower, "x", int64(len(content)))
	if got, err := os.ReadFile(filepath.Join(lower, "x")); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("got %q, %v", got, err)
	}
}

// inNamespace runs script in a new mount namespace, where it sees copies of
// our mounts that carry no mount marks (what containers and flatpak get).
func inNamespace(t *testing.T, script string) ([]byte, error) {
	t.Helper()
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip(err)
	}
	return exec.Command("unshare", "-m", "sh", "-c", script).CombinedOutput()
}

func TestHydrateFromOtherMountNamespace(t *testing.T) {
	content := []byte("not zeros")
	h := &mapHandler{data: map[string][]byte{"f/dir/a": content, "f/dir/b": content}}
	lower, view, l := setup(t, h, nil)
	os.Mkdir(filepath.Join(lower, "dir"), 0o755)
	placeholder(t, l, lower, "dir/a", int64(len(content)))
	placeholder(t, l, lower, "dir/b", int64(len(content)))
	elsewhere := t.TempDir()

	// Through the namespace's copy of the view.
	out, err := inNamespace(t, "cat "+filepath.Join(view, "dir/a"))
	if err != nil || !bytes.Equal(out, content) {
		t.Fatalf("copy of the view: got %q, %v", out, err)
	}
	// Through a container-style bind of the folder, after a local move:
	// found by the trailing part of the path.
	if err := os.Rename(filepath.Join(lower, "dir/b"), filepath.Join(lower, "dir/c")); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.data["f/dir/c"] = content
	h.mu.Unlock()
	out, err = inNamespace(t, "mount --bind "+view+" "+elsewhere+" && cat "+filepath.Join(elsewhere, "dir/c"))
	if err != nil || !bytes.Equal(out, content) {
		t.Fatalf("bind in another namespace: got %q, %v", out, err)
	}
}

func TestUnmatchedPathFailsClosed(t *testing.T) {
	h := &mapHandler{data: map[string][]byte{"f/dir/b": []byte("x")}}
	lower, view, l := setup(t, h, nil)
	os.Mkdir(filepath.Join(lower, "dir"), 0o755)
	placeholder(t, l, lower, "dir/a", 1)
	// Moved, then reached through a bind of its directory: neither its
	// origin nor any part of the path names it.
	os.Rename(filepath.Join(lower, "dir/a"), filepath.Join(lower, "dir/b"))
	elsewhere := t.TempDir()
	out, err := inNamespace(t, "mount --bind "+filepath.Join(view, "dir")+" "+elsewhere+" && cat "+filepath.Join(elsewhere, "b"))
	if err == nil {
		t.Fatalf("read succeeded with %q; want an error, never zeros", out)
	}
	if h.calls.Load() != 0 {
		t.Fatal("hydrated under a guessed name")
	}
}

func TestMarkTreeMarksExistingPlaceholders(t *testing.T) {
	content := []byte("from before")
	h := &mapHandler{data: map[string][]byte{"f/old": content}}
	lower, view, l := setup(t, h, nil)
	placeholder(t, nil, lower, "old", int64(len(content))) // unmarked, as after a restart
	os.WriteFile(filepath.Join(lower, "plain"), []byte("p"), 0o644)
	// Restarting the view marks it.
	if err := l.RemoveView(view); err != nil {
		t.Fatal(err)
	}
	if err := l.AddView(&View{Folder: "f", Lower: lower, Path: view}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(view, "old")); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, _ := os.ReadFile(filepath.Join(view, "plain")); string(got) != "p" || h.calls.Load() != 1 {
		t.Fatal("ordinary file went through the handler")
	}
}

func TestReadOnlyMountAccess(t *testing.T) {
	content := []byte("read-only mount")
	h := &mapHandler{data: map[string][]byte{"f/ro": content, "f/after": content}}
	lower, view, l := setup(t, h, nil)
	placeholder(t, l, lower, "ro", int64(len(content)))
	placeholder(t, l, lower, "after", int64(len(content)))
	elsewhere := t.TempDir()
	// docker -v ...:ro
	out, err := inNamespace(t, "mount --bind "+view+" "+elsewhere+" && mount -o remount,bind,ro "+elsewhere+
		" && cat "+filepath.Join(elsewhere, "ro"))
	if err != nil || !bytes.Equal(out, content) {
		t.Fatalf("through a read-only mount: got %q, %v", out, err)
	}
	// The listener is still serving.
	if got, err := os.ReadFile(filepath.Join(view, "after")); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("after: got %q, %v", got, err)
	}
}

// A read-only file (git objects are 0444) is written by its owner without
// CAP_DAC_OVERRIDE, and keeps its mode. Runs itself as nobody.
func TestOpenForWriteReadOnlyFile(t *testing.T) {
	if path := os.Getenv("TETHER_OFW_PATH"); path != "" {
		f, restore, err := OpenPathForWrite(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte("written"), 0); err != nil {
			t.Fatal(err)
		}
		restore()
		f.Close()
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		t.Skip(err)
	}
	dir, err := os.MkdirTemp("", "ofw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	const nobody = 65534
	path := filepath.Join(dir, "obj")
	if err := os.WriteFile(path, []byte("xxxxxxx"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, path} {
		os.Chown(p, nobody, nobody)
	}
	os.Chmod(dir, 0o755)
	cmd := exec.Command(setpriv, "--reuid=65534", "--regid=65534", "--clear-groups", "--inh-caps=-all",
		"--bounding-set=-all", os.Args[0], "-test.run=^TestOpenForWriteReadOnlyFile$")
	cmd.Env = append(os.Environ(), "TETHER_OFW_PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("as nobody: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if string(got) != "written" || st.Mode().Perm() != 0o444 {
		t.Fatalf("content %q, mode %v", got, st.Mode().Perm())
	}
}

// blockingHandler takes events and never answers them, like a sync process
// about to crash.
type blockingHandler struct{ took chan string }

func (h blockingHandler) Hydrate(ctx context.Context, _, name string, _ *os.File) error {
	h.took <- name
	<-ctx.Done()
	return ctx.Err()
}

// The group outlives a listener that dies mid-hydration (the monitor holds
// it): the interrupted access fails with EIO, accesses in the gap wait, and
// the next listener serves them. Nothing reads zeros.
func TestGroupSurvivesListener(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	g, err := NewGroup()
	if err != nil {
		t.Skip(err)
	}
	defer unix.Close(g)
	t.Setenv(GroupEnv, strconv.Itoa(g))
	base := t.TempDir()
	lower, view := filepath.Join(base, "lower"), filepath.Join(base, "view")
	os.MkdirAll(lower, 0o755)

	// Listener A takes x and dies without answering.
	blocked := blockingHandler{took: make(chan string, 1)}
	a, err := New(blocked, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.self.Store(-1)
	if err := a.AddView(&View{Folder: "f", Lower: lower, Path: view}); err != nil {
		t.Skipf("filesystem without HSM support? %v", err)
	}
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	go a.Serve(ctxA)
	content := []byte("survived")
	placeholder(t, a, lower, "x", int64(len(content)))
	placeholder(t, a, lower, "y", int64(len(content)))
	type result struct {
		out []byte
		err error
	}
	cat := func(path string) chan result {
		c := make(chan result, 1)
		go func() {
			out, err := exec.Command("cat", path).Output()
			c <- result{out, err}
		}()
		return c
	}
	interrupted := cat(filepath.Join(lower, "x"))
	if name := <-blocked.took; name != "x" {
		t.Fatalf("took %q", name)
	}
	a.Close() // the crash: x's event stays pending in the group

	gap := cat(filepath.Join(lower, "y"))
	select {
	case r := <-gap:
		t.Fatalf("access during the gap returned %q, %v; want it to wait", r.out, r.err)
	case <-time.After(500 * time.Millisecond):
	}

	// Listener B resumes the group.
	h := &mapHandler{data: map[string][]byte{"f/x": content, "f/y": content}}
	b, err := New(h, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b.self.Store(-1)
	if err := b.AddView(&View{Folder: "f", Lower: lower, Path: view}); err != nil {
		t.Fatal(err)
	}
	ctxB, cancelB := context.WithCancel(context.Background())
	defer func() { cancelB(); b.Close() }()
	go b.Serve(ctxB)

	if r := <-interrupted; r.err == nil {
		t.Fatalf("interrupted access returned %q; want an error, never zeros", r.out)
	}
	if r := <-gap; r.err != nil || !bytes.Equal(r.out, content) {
		t.Fatalf("access from the gap: %q, %v", r.out, r.err)
	}
	if got, err := os.ReadFile(filepath.Join(view, "x")); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("x after resuming: %q, %v", got, err)
	}
}

// While a placeholder's inode is unmarked (tether stopped), the guard fails
// opens with EIO instead of letting them read zeros; marked placeholders and
// ordinary files are unaffected.
func TestGuard(t *testing.T) {
	content := []byte("guarded")
	h := &mapHandler{data: map[string][]byte{"f/marked": content}}
	lower, view, l := setup(t, h, nil)
	dir := filepath.Join(t.TempDir(), "guard")
	if err := InstallGuard(dir); err != nil {
		t.Skip(err)
	}
	defer unix.Unmount(dir, unix.MNT_DETACH)
	defer RemoveGuard(dir)
	// Installing again replaces it.
	if err := InstallGuard(dir); err != nil {
		t.Fatal(err)
	}

	placeholder(t, l, lower, "marked", int64(len(content)))
	placeholder(t, l, lower, "later", int64(len(content)))
	placeholder(t, nil, lower, "unmarked", 10)
	os.WriteFile(filepath.Join(lower, "plain"), []byte("plain"), 0o644)

	if got, err := os.ReadFile(filepath.Join(view, "marked")); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("marked placeholder: %q, %v", got, err)
	}
	if _, err := os.ReadFile(filepath.Join(lower, "unmarked")); !errors.Is(err, unix.EIO) {
		t.Fatalf("unmarked placeholder: %v, want EIO", err)
	}
	if got, err := os.ReadFile(filepath.Join(lower, "plain")); err != nil || string(got) != "plain" {
		t.Fatalf("ordinary file: %q, %v", got, err)
	}
	// Tether stops: its marks go with the group, which the kernel tears
	// down asynchronously.
	l.Close()
	start := time.Now()
	var err error
	for _, err = os.ReadFile(filepath.Join(lower, "later")); !errors.Is(err, unix.EIO) && time.Since(start) < 2*time.Second; _, err = os.ReadFile(filepath.Join(lower, "later")) {
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(err, unix.EIO) {
		t.Fatalf("placeholder after the listener closed: %v, want EIO", err)
	}
	t.Logf("guarded %v after the listener closed", time.Since(start).Round(time.Millisecond))
	// Without the guard it would have read zeros. The kernel detaches it
	// asynchronously too (after an RCU grace period).
	if err := RemoveGuard(dir); err != nil {
		t.Fatal(err)
	}
	if n := attachedGuards(t); n > 0 {
		// The guard is system wide: one installed by a running tether
		// (not by this test) keeps guarding our placeholder.
		t.Skipf("%d other placeholder guard(s) attached on this system", n)
	}
	start = time.Now()
	var got []byte
	for got, err = os.ReadFile(filepath.Join(lower, "later")); err != nil && time.Since(start) < 5*time.Second; got, err = os.ReadFile(filepath.Join(lower, "later")) {
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || !bytes.Equal(got, make([]byte, len(content))) {
		t.Fatalf("after removing the guard: %q, %v", got, err)
	}
	t.Logf("guard detached %v after removal", time.Since(start).Round(time.Millisecond))
}

func TestDiscardAndRetarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	t0 := time.Unix(1_700_000_000, 123456789)
	if err := os.Chtimes(path, t0, t0); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := MarkVirtual(f, 1<<20, "f", []byte("bh")); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) {
			t.Skip("no user xattrs here")
		}
		t.Fatal(err)
	}
	check := func(what string, size int64, mtime time.Time) {
		t.Helper()
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			t.Fatal(err)
		}
		if st.Size != size || !time.Unix(st.Mtim.Unix()).Equal(mtime) || st.Blocks > 8 {
			t.Fatalf("%s: size %d mtime %v blocks %d, want %d %v 0", what, st.Size, time.Unix(st.Mtim.Unix()), st.Blocks, size, mtime)
		}
		if Interrupted(path) {
			t.Fatalf("%s: hydration marker left", what)
		}
		if !IsVirtual(f) {
			t.Fatalf("%s: no longer a placeholder", what)
		}
	}
	check("placeholder", 1<<20, t0)

	// A failed hydration: some content written, the mtime bumped. A second
	// attempt must not overwrite the recorded mtime.
	for range 2 {
		if mtime, err := BeginHydration(f); err != nil || !mtime.Equal(t0) {
			t.Fatal(mtime, err)
		}
		if _, err := f.WriteAt(bytes.Repeat([]byte("y"), 256<<10), 128<<10); err != nil {
			t.Fatal(err)
		}
		if !Interrupted(path) {
			t.Fatal("no hydration marker")
		}
	}
	if err := Discard(f); err != nil {
		t.Fatal(err)
	}
	check("discarded", 1<<20, t0)
	if err := Discard(f); err != nil {
		t.Fatal("discard without marker:", err)
	}

	t1 := time.Unix(1_800_000_000, 5)
	if _, err := BeginHydration(f); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("z"), 0); err != nil {
		t.Fatal(err)
	}
	if err := Retarget(f, 3000, "g", []byte("bh2"), t1); err != nil {
		t.Fatal(err)
	}
	check("retargeted", 3000, t1)
	if origin, bh, ok := ReadPlaceholder(path); !ok || origin != "g" || string(bh) != "bh2" {
		t.Fatalf("retargeted: %q %q %v", origin, bh, ok)
	}
}

// attachedGuards counts placeholder guard programs attached by others. Ours
// may linger for a moment after RemoveGuard, so this waits for the count
// to settle.
func attachedGuards(t *testing.T) int {
	count := func() int {
		n := 0
		it := new(link.Iterator)
		defer it.Close()
		for it.Next() {
			info, err := it.Link.Info()
			if err != nil || info.Type != link.TracingType {
				continue
			}
			p, err := ebpf.NewProgramFromID(info.Program)
			if err != nil {
				continue
			}
			pi, err := p.Info()
			p.Close()
			if err == nil && pi.Name == guardProgram {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(5 * time.Second)
	n := count()
	for n > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		n = count()
	}
	return n
}

// An open that races with an eviction waits for its lease, but the kernel
// decides whether the file raises pre-content events before that wait. It
// must not read the placeholder's zeros.
func TestEvictRacingOpen(t *testing.T) {
	content := bytes.Repeat([]byte("racing open "), 1000)
	h := &mapHandler{data: map[string][]byte{"f/r": content}}
	lower, view, l := setup(t, h, nil)
	if err := os.WriteFile(filepath.Join(lower, "r"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 1)
	start := time.Now()
	err := l.Evict("f", "r", int64(len(content)), "r", []byte("bh"), func(*os.File) error {
		go func() {
			b, err := os.ReadFile(filepath.Join(view, "r"))
			if err != nil {
				b = []byte(err.Error())
			}
			got <- b
		}()
		time.Sleep(200 * time.Millisecond) // by now the open waits for our lease
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("eviction took %v", d)
	}
	b := <-got
	if !bytes.Equal(b, content) {
		t.Fatalf("racing reader got %d bytes (zeros: %v), want the content", len(b), len(b) > 0 && bytes.Count(b, []byte{0}) == len(b))
	}
	if h.calls.Load() != 1 {
		t.Fatalf("handler called %d times, want 1", h.calls.Load())
	}
}
