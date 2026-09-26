// Command probe reads a file through one specific kernel path and prints the
// SHA-256 of what it read. Used by the e2e tests inside minimal containers.
//
//	probe mmap FILE
//	probe odirect FILE
//	probe pread FILE OFFSET COUNT
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
	}
	return nil, fmt.Errorf("unknown mode %q", mode)
}
