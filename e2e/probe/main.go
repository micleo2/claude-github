// Command probe reads a file through one specific kernel path and prints the
// SHA-256 of what it read. Used by the e2e tests inside minimal containers.
//
//	probe mmap FILE
//	probe odirect FILE
//	probe pread FILE OFFSET COUNT
//	probe mkph FILE SIZE   (make a spike-style placeholder: sparse + user.tether.state=virtual)
package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: probe mmap|odirect|pread FILE [OFFSET COUNT]")
		os.Exit(2)
	}
	if os.Args[1] == "mkph" {
		if err := mkph(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	data, err := read(os.Args[1], os.Args[2], os.Args[3:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%x\n", sha256.Sum256(data))
}

func read(mode, path string, args []string) ([]byte, error) {
	switch mode {
	case "mmap":
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		st, _ := f.Stat()
		if st.Size() == 0 {
			return nil, nil
		}
		m, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return nil, err
		}
		defer syscall.Munmap(m)
		return append([]byte(nil), m...), nil
	case "odirect":
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECT, 0)
		if err != nil {
			return nil, err
		}
		defer syscall.Close(fd)
		var st syscall.Stat_t
		syscall.Fstat(fd, &st)
		// O_DIRECT needs an aligned buffer.
		raw := make([]byte, st.Size+2*4096)
		off := 4096 - int(uintptr(unsafe.Pointer(&raw[0]))%4096)
		buf := raw[off : off+int((st.Size+4095)/4096*4096)]
		var out []byte
		for pos := 0; pos < int(st.Size); {
			n, err := syscall.Pread(fd, buf[pos:], int64(pos))
			if err != nil {
				return nil, err
			}
			if n == 0 {
				break
			}
			pos += n
			out = buf[:pos]
		}
		return out[:st.Size], nil
	case "pread":
		off, _ := strconv.ParseInt(args[0], 10, 64)
		n, _ := strconv.Atoi(args[1])
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		buf := make([]byte, n)
		got, err := f.ReadAt(buf, off)
		if got == n {
			err = nil
		}
		return buf[:got], err
	case "seekdata", "fiemap", "copyrange", "sendfile":
		return copyLike(mode, path)
	}
	return nil, fmt.Errorf("unknown mode %q", mode)
}

// copyLike reads path the way sparse-aware copiers do, trusting the
// filesystem's view of which ranges hold data: a range it reports as a hole
// (or unwritten) is taken to be zeros without reading it.
func copyLike(mode, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fd := int(f.Fd())
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	out := make([]byte, size)
	readRange := func(off, end int64) error {
		for off < end {
			n, err := syscall.Pread(fd, out[off:end], off)
			if err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("short read at %d", off)
			}
			off += int64(n)
		}
		return nil
	}
	switch mode {
	case "seekdata": // GNU cp, tar -S, rsync -S
		const seekData, seekHole = 3, 4
		for off := int64(0); off < size; {
			data, err := syscall.Seek(fd, off, seekData)
			if err == syscall.ENXIO {
				break // the rest is a hole
			} else if err != nil {
				return nil, err
			}
			hole, err := syscall.Seek(fd, data, seekHole)
			if err != nil {
				return nil, err
			}
			if err := readRange(data, min(hole, size)); err != nil {
				return nil, err
			}
			off = hole
		}
	case "fiemap": // older cp: mapped, written extents only
		exts, err := fiemap(fd)
		if err != nil {
			return nil, err
		}
		for _, e := range exts {
			if e.flags&fiemapExtentUnwritten != 0 {
				continue
			}
			if err := readRange(int64(e.logical), min(int64(e.logical+e.length), size)); err != nil {
				return nil, err
			}
		}
	case "copyrange", "sendfile":
		tmp, err := os.CreateTemp("", "probe-copy")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		for done := int64(0); done < size; {
			var n int
			if mode == "copyrange" {
				n, err = copyFileRange(fd, int(tmp.Fd()), int(size-done))
			} else {
				n, err = syscall.Sendfile(int(tmp.Fd()), fd, nil, int(size-done))
			}
			if err != nil {
				return nil, err
			}
			if n == 0 {
				return nil, fmt.Errorf("%s stopped at %d", mode, done)
			}
			done += int64(n)
		}
		if _, err := tmp.ReadAt(out, 0); err != nil && size > 0 {
			return nil, err
		}
	}
	return out, nil
}

func copyFileRange(in, out, n int) (int, error) {
	r, _, errno := syscall.Syscall6(326, uintptr(in), 0, uintptr(out), 0, uintptr(n), 0) // copy_file_range, amd64
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

const fiemapExtentUnwritten = 0x800

type fiemapExtent struct {
	logical, physical, length uint64
	_                         [2]uint64
	flags                     uint32
	_                         [3]uint32
}

type fiemapHeader struct {
	start, length              uint64
	flags, mapped, count, _pad uint32
}

func fiemap(fd int) ([]fiemapExtent, error) {
	const n = 512
	buf := make([]byte, unsafe.Sizeof(fiemapHeader{})+n*unsafe.Sizeof(fiemapExtent{}))
	h := (*fiemapHeader)(unsafe.Pointer(&buf[0]))
	h.length = ^uint64(0)
	h.flags = 1 // FIEMAP_FLAG_SYNC
	h.count = n
	const fsIocFiemap = 0xc020660b
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), fsIocFiemap, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return nil, errno
	}
	exts := unsafe.Slice((*fiemapExtent)(unsafe.Pointer(&buf[unsafe.Sizeof(fiemapHeader{})])), h.mapped)
	return append([]fiemapExtent(nil), exts...), nil
}

func mkph(path, size string) error {
	n, err := strconv.ParseInt(size, 10, 64)
	if err != nil {
		return err
	}
	os.Remove(path)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// Same attributes as a real tether placeholder, so disk cost is realistic.
	for _, xa := range [][2]string{
		{"user.tether.state", "virtual"},
		{"user.tether.origin", "some/typical/directory/" + path},
		{"user.tether.bh", string(make([]byte, 32))},
	} {
		if err := syscall.Setxattr(path, xa[0], []byte(xa[1]), 0); err != nil {
			return err
		}
	}
	return f.Truncate(n)
}
