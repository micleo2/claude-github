// Startup cost of per-placeholder inode marks. Usage: markbench <dir> <n>
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func meminfo() map[string]int64 {
	m := map[string]int64{}
	f, _ := os.Open("/proc/meminfo")
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fs := strings.Fields(s.Text())
		v, _ := strconv.ParseInt(fs[1], 10, 64)
		m[strings.TrimSuffix(fs[0], ":")] = v
	}
	return m
}

func dropCaches() {
	unix.Sync()
	os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0)
}

func main() {
	dir := os.Args[1]
	n, _ := strconv.Atoi(os.Args[2])
	paths := make([]string, n)
	t := time.Now()
	for i := range paths {
		d := filepath.Join(dir, fmt.Sprintf("d%04d/s%03d", i/10000, i/100%100))
		if i%100 == 0 {
			os.MkdirAll(d, 0o755)
		}
		p := filepath.Join(d, fmt.Sprintf("file-%07d.cc", i))
		paths[i] = p
		f, err := os.Create(p)
		if err != nil {
			panic(err)
		}
		f.Truncate(4096 + int64(i%50000))
		f.Close()
		unix.Setxattr(p, "user.tether.state", []byte("virtual:0123456789abcdef0123456789abcdef"), 0)
	}
	fmt.Printf("created %d placeholders in %v\n", n, time.Since(t).Round(time.Millisecond))

	run := func(label string, cold bool, mark bool) {
		if cold {
			dropCaches()
		}
		fd, err := unix.FanotifyInit(unix.FAN_CLASS_PRE_CONTENT|unix.FAN_CLOEXEC|unix.FAN_UNLIMITED_QUEUE|unix.FAN_UNLIMITED_MARKS, unix.O_RDONLY)
		if err != nil {
			panic(err)
		}
		m0 := meminfo()
		t := time.Now()
		var st unix.Stat_t
		for _, p := range paths {
			if mark {
				if err := unix.FanotifyMark(fd, unix.FAN_MARK_ADD, unix.FAN_OPEN_PERM|unix.FAN_PRE_ACCESS, unix.AT_FDCWD, p); err != nil {
					panic(err)
				}
			} else {
				unix.Lstat(p, &st)
			}
		}
		el := time.Since(t)
		m1 := meminfo()
		slab := m1["Slab"] - m0["Slab"]
		fmt.Printf("  %-22s %8v  %7.0f/s  slab +%d MiB (%d B/file)\n", label, el.Round(time.Millisecond),
			float64(n)/el.Seconds(), slab/1024, slab*1024/int64(n))
		unix.Close(fd) // marks go away with the group
	}
	run("stat all, cold", true, false)
	run("mark all, cold", true, true)
	run("stat all, warm", false, false)
	run("mark all, warm", false, true)
}
