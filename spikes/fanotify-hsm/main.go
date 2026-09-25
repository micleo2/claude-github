// Command fanotify-hsm is the Phase 0 spike: a minimal fanotify pre-content
// (HSM) listener that lazily fills placeholder files from a "remote"
// directory. It exists to validate kernel behaviour before the mechanism is
// wired into the sync daemon.
//
//	fanotify-hsm -mount /mnt/root -remote /srv/remote [-block 65536] [-ignore]
//
// A placeholder is a sparse file carrying xattr user.tether.state=virtual.
// When fully filled the xattr is set to "local".
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	xattrState = "user.tether.state"

	// FS_IOC_GETVERSION: _IOR('v', 1, long)
	fsIocGetVersion = 0x80087601
)

var (
	mountPath  = flag.String("mount", "", "mount point to watch")
	remotePath = flag.String("remote", "", "directory holding the real content")
	blockSize  = flag.Int64("block", 64<<10, "hydration granularity")
	useIgnore  = flag.Bool("ignore", true, "add evictable ignore marks for hydrated files")
	denyErrno  = flag.Int("deny-missing", int(unix.EIO), "errno returned when the remote copy is missing")
	verbose    = flag.Bool("v", false, "log every event")
	fullBelow  = flag.Int64("full-below", 64<<20, "hydrate files up to this size completely at open")
	markType   = flag.String("mark", "mount", "mark type: mount, filesystem or inode (each file directly under -mount)")
)

type fileState struct {
	filled map[int64]bool // block index -> filled
}

// fileKey identifies an inode incarnation. Paths are useless as keys (a
// placeholder can be deleted and recreated under the same name) and inode
// numbers get reused, so include the generation number.
type fileKey struct {
	dev, ino uint64
	gen      int
}

var (
	mu    sync.Mutex
	files = map[fileKey]*fileState{}
)

// sparseCopiers skip holes via SEEK_DATA/FICLONE, which fires no pre-content
// event, so they must see a fully hydrated file.
var sparseCopiers = map[string]bool{
	"cp": true, "mv": true, "install": true, "rsync": true, "tar": true,
	"bsdtar": true, "cpio": true, "ditto": true,
}

func main() {
	flag.Parse()
	if *mountPath == "" || *remotePath == "" {
		flag.Usage()
		os.Exit(2)
	}

	fd, err := unix.FanotifyInit(unix.FAN_CLASS_PRE_CONTENT|unix.FAN_CLOEXEC|unix.FAN_UNLIMITED_QUEUE, unix.O_RDWR|unix.O_LARGEFILE)
	if err != nil {
		log.Fatalf("fanotify_init: %v", err)
	}
	// FAN_OPEN_PERM lets us hydrate whole files before tools that probe
	// holes with SEEK_DATA/SEEK_HOLE (cp, rsync -S, tar -S) or FICLONE get a
	// chance to skip the "empty" placeholder without ever reading it.
	mask := uint64(unix.FAN_PRE_ACCESS | unix.FAN_OPEN_PERM)
	switch *markType {
	case "mount", "filesystem":
		flags := uint(unix.FAN_MARK_MOUNT)
		if *markType == "filesystem" {
			flags = unix.FAN_MARK_FILESYSTEM
		}
		if err := unix.FanotifyMark(fd, unix.FAN_MARK_ADD|flags, mask, unix.AT_FDCWD, *mountPath); err != nil {
			log.Fatalf("fanotify_mark(%s): %v", *markType, err)
		}
	case "inode":
		ents, _ := os.ReadDir(*mountPath)
		for _, e := range ents {
			if err := unix.FanotifyMark(fd, unix.FAN_MARK_ADD, mask, unix.AT_FDCWD, filepath.Join(*mountPath, e.Name())); err != nil {
				log.Printf("fanotify_mark(inode %s): %v", e.Name(), err)
			}
		}
	}
	log.Printf("listening on %s (pid %d)", *mountPath, os.Getpid())
	fmt.Println("READY")

	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(fd, buf)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			log.Fatalf("read: %v", err)
		}
		for off := 0; off < n; {
			md := (*unix.FanotifyEventMetadata)(unsafe.Pointer(&buf[off]))
			ev := buf[off : off+int(md.Event_len)]
			off += int(md.Event_len)
			handle(fd, md, ev)
		}
	}
}

func handle(group int, md *unix.FanotifyEventMetadata, ev []byte) {
	evfd := int(md.Fd)
	defer unix.Close(evfd)

	var rOff, rCount uint64
	haveRange := false
	for p := int(md.Metadata_len); p+4 <= len(ev); {
		typ := ev[p]
		l := int(binary.LittleEndian.Uint16(ev[p+2:]))
		if l == 0 {
			break
		}
		if typ == unix.FAN_EVENT_INFO_TYPE_RANGE && p+24 <= len(ev) {
			rOff = binary.LittleEndian.Uint64(ev[p+8:])
			rCount = binary.LittleEndian.Uint64(ev[p+16:])
			haveRange = true
		}
		p += l
	}

	path, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", evfd))
	comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", md.Pid))
	if *verbose {
		log.Printf("event mask=%#x pid=%d(%s) path=%s range=%v off=%d count=%d",
			md.Mask, md.Pid, strings.TrimSpace(string(comm)), path, haveRange, rOff, rCount)
	}

	resp := uint32(unix.FAN_ALLOW)
	var err error
	if md.Mask&unix.FAN_OPEN_PERM != 0 {
		err = openPerm(group, evfd, path, int(md.Pid))
	} else {
		err = fill(group, evfd, path, haveRange, rOff, rCount)
	}
	if err != nil {
		log.Printf("fill %s: %v", path, err)
		var en unix.Errno
		code := unix.Errno(*denyErrno)
		if errors.As(err, &en) {
			code = en
		}
		resp = unix.FAN_DENY | (uint32(code)&unix.FAN_ERRNO_MASK)<<unix.FAN_ERRNO_SHIFT
	}
	var r [8]byte
	binary.LittleEndian.PutUint32(r[0:], uint32(md.Fd))
	binary.LittleEndian.PutUint32(r[4:], resp)
	if _, err := unix.Write(group, r[:]); err != nil {
		log.Printf("respond: %v", err)
	}
}

func state(fd int) string {
	b := make([]byte, 32)
	n, err := unix.Fgetxattr(fd, xattrState, b)
	if err != nil {
		return ""
	}
	return string(b[:n])
}

func openPerm(group, fd int, path string, pid int) error {
	if state(fd) != "virtual" {
		return nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if st.Size <= *fullBelow || sparseCopiers[filepath.Base(exe)] {
		return fill(group, fd, path, false, 0, 0)
	}
	return nil
}

func fill(group, fd int, path string, haveRange bool, off, count uint64) error {
	if state(fd) != "virtual" {
		// Not ours or already hydrated: allow, and stop hearing about it.
		ignore(group, fd)
		return nil
	}
	rel, err := filepath.Rel(*mountPath, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}
	src, err := os.Open(filepath.Join(*remotePath, rel))
	if err != nil {
		return unix.Errno(*denyErrno)
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}

	gen, _ := unix.IoctlGetInt(fd, fsIocGetVersion)
	key := fileKey{dev: st.Dev, ino: st.Ino, gen: gen}

	mu.Lock()
	defer mu.Unlock()
	fs := files[key]
	if fs == nil {
		fs = &fileState{filled: map[int64]bool{}}
		files[key] = fs
	}

	first, last := int64(0), (size-1) / *blockSize
	if haveRange && count > 0 {
		first = int64(off) / *blockSize
		last = (int64(off+count) - 1) / *blockSize
	}
	if size == 0 {
		last = -1
	}
	if max := (size - 1) / *blockSize; last > max {
		last = max
	}
	buf := make([]byte, *blockSize)
	for b := first; b <= last; b++ {
		if fs.filled[b] {
			continue
		}
		n, err := src.ReadAt(buf, b**blockSize)
		if err != nil && n == 0 {
			return err
		}
		if _, err := unix.Pwrite(fd, buf[:n], b**blockSize); err != nil {
			return err
		}
		fs.filled[b] = true
	}
	// Writing through the event fd bumps mtime; restore it so the file still
	// looks untouched to scanners.
	ts := []unix.Timespec{unix.NsecToTimespec(st.Atim.Nano()), unix.NsecToTimespec(st.Mtim.Nano())}
	_ = unix.UtimesNanoAt(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), ts, 0)

	if int64(len(fs.filled))*(*blockSize) >= size {
		if err := unix.Fsetxattr(fd, xattrState, []byte("local"), 0); err != nil {
			return err
		}
		delete(files, key)
		ignore(group, fd)
	}
	return nil
}

func ignore(group, fd int) {
	if !*useIgnore {
		return
	}
	err := unix.FanotifyMark(group, unix.FAN_MARK_ADD|unix.FAN_MARK_IGNORE_SURV|unix.FAN_MARK_EVICTABLE,
		unix.FAN_PRE_ACCESS|unix.FAN_OPEN_PERM, fd, "")
	if err != nil && *verbose {
		log.Printf("ignore mark: %v", err)
	}
}
