// Which fanotify marks see placeholder reads from every mount and mount
// namespace? MODE=inode marks each placeholder's inode, MODE=sb marks the
// filesystem. Either way the daemon works through a detached mount carrying
// an ignore mark. Usage: MODE=inode|sb coverage <mountpoint>  (root)
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const mask = unix.FAN_OPEN_PERM | unix.FAN_PRE_ACCESS

var events atomic.Int64
var fail int

func check(desc string, ok bool, detail string) {
	st := "PASS"
	if !ok {
		st, fail = "FAIL", fail+1
	}
	fmt.Printf("%s  %s  %s\n", st, desc, detail)
}

func main() {
	mnt := os.Args[1]
	must := func(err error, what string) {
		if err != nil {
			fmt.Println("FATAL", what, err)
			os.Exit(2)
		}
	}
	content := []byte("REAL CONTENT from the peer\n")
	for _, n := range []string{"p-own", "p-ns", "p-bind", "p-plain", "p-docker"} {
		p := filepath.Join(mnt, n)
		os.Remove(p)
		must(os.WriteFile(p, nil, 0o644), "create")
		must(os.Truncate(p, int64(len(content))), "truncate")
		must(unix.Setxattr(p, "user.tether.state", []byte("virtual"), 0), "xattr")
	}

	fd, err := unix.FanotifyInit(unix.FAN_CLASS_PRE_CONTENT|unix.FAN_CLOEXEC, unix.O_RDWR)
	must(err, "fanotify_init")
	if os.Getenv("MODE") == "inode" {
		// Mark each placeholder's inode; nothing else on the filesystem.
		for _, n := range []string{"p-own", "p-ns", "p-bind", "p-plain", "p-docker"} {
			must(unix.FanotifyMark(fd, unix.FAN_MARK_ADD, mask, unix.AT_FDCWD, filepath.Join(mnt, n)), "inode mark")
		}
		fmt.Println("mode: per-placeholder inode marks")
	} else {
		must(unix.FanotifyMark(fd, unix.FAN_MARK_ADD|unix.FAN_MARK_FILESYSTEM, mask, unix.AT_FDCWD, mnt), "sb mark")
		fmt.Println("mode: filesystem mark")
	}

	tree, err := unix.OpenTree(unix.AT_FDCWD, mnt, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	must(err, "open_tree")
	must(unix.FanotifyMark(fd, unix.FAN_MARK_ADD|unix.FAN_MARK_MOUNT|unix.FAN_MARK_IGNORE_SURV, mask, tree, "."), "mount ignore mark")
	own := fmt.Sprintf("/proc/self/fd/%d", tree) // the daemon's private access path

	hydrated := atomic.Int64{}
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := unix.Read(fd, buf)
			if err != nil {
				return
			}
			for off := 0; off < n; {
				var m unix.FanotifyEventMetadata
				binary.Read(bytes.NewReader(buf[off:off+24]), binary.LittleEndian, &m)
				events.Add(1)
				name, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", m.Fd))
				base := filepath.Base(name)
				// Hydrate through the daemon's own detached mount.
				var v [16]byte
				if sz, err := unix.Getxattr(filepath.Join(own, base), "user.tether.state", v[:]); err == nil && string(v[:sz]) == "virtual" {
					f, err := os.OpenFile(filepath.Join(own, base), os.O_WRONLY, 0)
					if err == nil {
						f.WriteAt(content, 0)
						f.Close()
						unix.Removexattr(filepath.Join(own, base), "user.tether.state")
						hydrated.Add(1)
					}
				}
				// Exempt the (now local) inode from further events.
				if os.Getenv("MODE") == "inode" {
					unix.FanotifyMark(fd, unix.FAN_MARK_REMOVE, mask, int(m.Fd), "")
				} else {
					unix.FanotifyMark(fd, unix.FAN_MARK_ADD|unix.FAN_MARK_IGNORE_SURV|unix.FAN_MARK_EVICTABLE, mask, int(m.Fd), "")
				}
				var r [8]byte
				binary.LittleEndian.PutUint32(r[0:], uint32(m.Fd))
				binary.LittleEndian.PutUint32(r[4:], unix.FAN_ALLOW)
				unix.Write(fd, r[:])
				unix.Close(int(m.Fd))
				off += int(m.Event_len)
			}
		}
	}()

	// 1. The daemon's own access through the ignored detached mount.
	e0 := events.Load()
	b, err := os.ReadFile(filepath.Join(own, "p-own"))
	check("daemon's detached mount generates no events", err == nil && events.Load() == e0,
		fmt.Sprintf("events=%d zeros=%v", events.Load()-e0, bytes.Equal(b, make([]byte, len(content)))))

	read := func(desc string, cmd *exec.Cmd) {
		h0 := hydrated.Load()
		out, err := cmd.CombinedOutput()
		check(desc, err == nil && bytes.Equal(out, content) && hydrated.Load() == h0+1,
			fmt.Sprintf("err=%v out=%q", err, strings.TrimSpace(string(out))))
	}
	// 2. A new mount namespace: a full copy of the mount tree (unshare, bwrap, docker).
	read("new mount namespace (unshare -m) hydrates", exec.Command("unshare", "-m", "cat", filepath.Join(mnt, "p-ns")))
	// 3. A bind mount made after the mark.
	bind := mnt + "-bind"
	os.MkdirAll(bind, 0o755)
	must(unix.Mount(mnt, bind, "", unix.MS_BIND, ""), "bind")
	read("later bind mount hydrates", exec.Command("cat", filepath.Join(bind, "p-bind")))
	unix.Unmount(bind, unix.MNT_DETACH)
	// 4. The ordinary mount path.
	read("ordinary path hydrates", exec.Command("cat", filepath.Join(mnt, "p-plain")))
	// 5. Namespace + bind, the way container runtimes expose host paths.
	read("unshare + bind inside the namespace hydrates", exec.Command("unshare", "-m", "sh", "-c",
		fmt.Sprintf("mkdir -p /tmp/cx && mount --bind %s /tmp/cx && cat /tmp/cx/p-docker", mnt)))

	// 6. After hydration the inode is exempt: re-reading generates no events.
	e1 := events.Load()
	for i := 0; i < 100; i++ {
		os.ReadFile(filepath.Join(mnt, "p-plain"))
	}
	check("hydrated file re-read: no further events", events.Load() == e1, fmt.Sprintf("events=%d", events.Load()-e1))

	// 7. Cost on a non-placeholder file: first open traps once, then exempt.
	os.WriteFile(filepath.Join(own, "normal"), []byte("x"), 0o644)
	t0 := time.Now()
	e2 := events.Load()
	for i := 0; i < 1000; i++ {
		os.ReadFile(filepath.Join(mnt, "normal"))
	}
	check("normal file, 1000 reads", events.Load()-e2 <= 2, fmt.Sprintf("events=%d total=%v", events.Load()-e2, time.Since(t0)))
	os.Exit(fail)
}
