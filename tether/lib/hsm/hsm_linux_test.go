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
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	return Finish(f, time.Unix(1700000000, 0))
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
	if err := l.AddView(&View{Folder: "f", Lower: lower, Path: view}); err != nil {
		t.Skipf("cannot create view (filesystem without HSM support?): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go l.Serve(ctx)
	t.Cleanup(func() { cancel(); l.Close() })
	return lower, view, l
}

func placeholder(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := MarkVirtual(f, size, filepath.Base(path), []byte("bh")); err != nil {
		t.Fatal(err)
	}
}

func TestHydrateOnOpen(t *testing.T) {
	content := bytes.Repeat([]byte("tether!"), 100000)
	h := &mapHandler{data: map[string][]byte{"f/dir/a": content}}
	lower, view, _ := setup(t, h, nil)
	os.Mkdir(filepath.Join(lower, "dir"), 0o755)
	placeholder(t, filepath.Join(lower, "dir/a"), int64(len(content)))

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
	lower, view, _ := setup(t, h, nil)
	placeholder(t, filepath.Join(lower, "big"), int64(len(content)))
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
	lower, view, _ := setup(t, h, nil)
	placeholder(t, filepath.Join(lower, "x"), int64(len(content)))
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
	lower, view, _ := setup(t, h, nil)
	placeholder(t, filepath.Join(lower, "missing"), 10)
	_, err := os.ReadFile(filepath.Join(view, "missing"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestPolicyDenies(t *testing.T) {
	h := &mapHandler{data: map[string][]byte{"f/x": []byte("hi")}}
	lower, view, _ := setup(t, h, func(folder string, pid int, exe string) unix.Errno {
		if filepath.Base(exe) == "denied-cat" {
			return unix.EAGAIN
		}
		return 0
	})
	placeholder(t, filepath.Join(lower, "x"), 2)
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
	placeholder(t, filepath.Join(lower, "r"), int64(len(content)))
	if got, _ := os.ReadFile(filepath.Join(view, "r")); !bytes.Equal(got, content) {
		t.Fatal("first hydration")
	}

	// Evict: lease, punch, forget ignore mark.
	f, err := os.OpenFile(filepath.Join(lower, "r"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := Lease(f); err != nil {
		t.Fatal(err)
	}
	if err := MarkVirtual(f, int64(len(content)), "r", []byte("bh")); err != nil {
		t.Fatal(err)
	}
	if err := l.Forget(f); err != nil {
		t.Fatal(err)
	}
	Unlease(f)
	f.Close()

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
