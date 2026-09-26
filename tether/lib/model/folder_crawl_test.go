// Copyright (C) 2026 The tether Authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package model

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCommonDir(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"a/b/c", "a/b/d", "a/b"},
		{"a/b", "a/b/c", "a/b"},
		{"a/b/c", "a/b", "a/b"},
		{"a", "b", ""},
		{"", "a/b", ""},
		{"ab/c", "a/c", ""},
		{"a/b", "a/b", "a/b"},
	}
	for _, c := range cases {
		if got := commonDir(c.a, c.b); got != c.want {
			t.Errorf("commonDir(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

// walkAll returns what find(1) would list below root, in its order.
func findOrder(t *testing.T, base, root string) []string {
	t.Helper()
	var out []string
	var walk func(dir string)
	walk = func(dir string) {
		d, err := os.Open(filepath.Join(base, dir))
		if err != nil {
			t.Fatal(err)
		}
		entries, _ := d.ReadDir(-1)
		d.Close()
		for _, e := range entries {
			name := filepath.Join(dir, e.Name())
			if e.IsDir() {
				walk(name)
			} else {
				out = append(out, name)
			}
		}
	}
	walk(root)
	return out
}

func TestTreeWalker(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{
		"top.txt",
		"a/1", "a/2", "a/x/1", "a/x/2", "a/y/1", "a/3",
		"b/1", "b/z/1",
		".stfolder/marker",
	} {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a", filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	drain := func(w *treeWalker) []string {
		var out []string
		for {
			name, ok := w.next()
			if !ok {
				return out
			}
			out = append(out, name)
		}
	}

	all := findOrder(t, base, "")
	all = slices.DeleteFunc(all, func(s string) bool { return s == ".stfolder/marker" || s == "link" })
	if got := drain(newTreeWalker(base, "", "")); !slices.Equal(got, all) {
		t.Fatalf("whole tree:\n got %v\nwant %v", got, all)
	}

	// Starting after each file continues exactly where find would.
	for i, after := range all {
		if got := drain(newTreeWalker(base, "", after)); !slices.Equal(got, all[i+1:]) {
			t.Errorf("after %s:\n got %v\nwant %v", after, got, all[i+1:])
		}
	}

	// Under a root, only its files.
	sub := findOrder(t, base, "a")
	if got := drain(newTreeWalker(base, "a", "")); !slices.Equal(got, sub) {
		t.Errorf("root a:\n got %v\nwant %v", got, sub)
	}
	if got := drain(newTreeWalker(base, "a", "b/1")); !slices.Equal(got, sub) {
		t.Errorf("start outside root: got %v, want %v", got, sub)
	}

	// A start file that no longer exists starts its directory over.
	if got := drain(newTreeWalker(base, "b", "b/gone")); !slices.Equal(got, findOrder(t, base, "b")) {
		t.Errorf("vanished start: got %v", got)
	}

	// position reports the last file handed out.
	w := newTreeWalker(base, "", "")
	for range 3 {
		w.next()
	}
	if got := drain(newTreeWalker(base, "", w.position())); !slices.Equal(got, drain(w)) {
		t.Errorf("position does not resume the walk")
	}
}

func TestPrefetchQueueLanes(t *testing.T) {
	q := newPrefetchQueue()
	q.pushLane(7, []string{"l7/a", "l7/b", "l7/c"})
	q.pushLane(9, []string{"l9/a", "l9/b"})
	q.push([]string{"sib/a", "l7/b"}) // l7/b is already queued

	var got []string
	var lanes []int
	for {
		name, lane, ok := q.pop()
		if !ok {
			break
		}
		got = append(got, name)
		lanes = append(lanes, lane)
	}
	// Siblings first, then the lanes round-robin.
	want := []string{"sib/a", "l7/a", "l9/a", "l7/b", "l9/b", "l7/c"}
	if !slices.Equal(got, want) {
		t.Fatalf("order: got %v, want %v", got, want)
	}
	if !slices.Equal(lanes, []int{0, 7, 9, 7, 9, 7}) {
		t.Fatalf("lanes: %v", lanes)
	}

	q.pushLane(7, []string{"x", "y"})
	q.pushLane(9, []string{"z"})
	q.dropLane(7)
	if q.len() != 1 {
		t.Fatalf("len after dropping a lane: %d", q.len())
	}
	if name, lane, _ := q.pop(); name != "z" || lane != 9 {
		t.Fatalf("got %s from %d", name, lane)
	}
	// A dropped lane's names can be queued again.
	if n := q.pushLane(9, []string{"x"}); n != 1 {
		t.Fatal("dropped names stay deduplicated")
	}
}
