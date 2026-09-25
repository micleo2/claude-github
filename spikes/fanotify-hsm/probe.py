#!/usr/bin/env python3
"""Access a file with one specific kernel path and print the sha256 of what was read.

usage: probe.py MODE FILE [args]
"""
import hashlib, mmap, os, sys

mode, path = sys.argv[1], sys.argv[2]
h = lambda b: hashlib.sha256(b).hexdigest()

def slurp_fd(fd):
    out = bytearray()
    while True:
        b = os.read(fd, 1 << 20)
        if not b:
            return bytes(out)
        out += b

try:
    if mode == "read":
        with open(path, "rb") as f:
            print(h(f.read()))
    elif mode == "pread":  # pread OFFSET COUNT
        off, cnt = int(sys.argv[3]), int(sys.argv[4])
        fd = os.open(path, os.O_RDONLY)
        print(h(os.pread(fd, cnt, off)))
    elif mode == "mmap-read":
        with open(path, "rb") as f:
            m = mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ)
            print(h(m[:]))
    elif mode == "mmap-write":  # write b"X" at offset, then print whole-file hash
        off = int(sys.argv[3])
        with open(path, "r+b") as f:
            m = mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_WRITE)
            m[off:off + 1] = b"X"
            m.flush()
            m.close()
        with open(path, "rb") as f:
            print(h(f.read()))
    elif mode == "pwrite":  # write b"X" at offset without reading first
        off = int(sys.argv[3])
        fd = os.open(path, os.O_WRONLY)
        os.pwrite(fd, b"X", off)
        os.close(fd)
        with open(path, "rb") as f:
            print(h(f.read()))
    elif mode == "append":
        with open(path, "ab") as f:
            f.write(b"TAIL")
        with open(path, "rb") as f:
            print(h(f.read()))
    elif mode == "truncate":  # truncate to N bytes
        os.truncate(path, int(sys.argv[3]))
        with open(path, "rb") as f:
            print(h(f.read()))
    elif mode == "sendfile":
        src = os.open(path, os.O_RDONLY)
        r, w = os.pipe()
        size = os.fstat(src).st_size
        out = bytearray()
        off = 0
        while off < size:
            n = os.sendfile(w, src, off, min(65536, size - off))
            off += n
            out += os.read(r, n)
        print(h(bytes(out)))
    elif mode == "splice":
        src = os.open(path, os.O_RDONLY)
        r, w = os.pipe()
        out = bytearray()
        while True:
            n = os.splice(src, w, 65536)
            if n == 0:
                break
            out += os.read(r, n)
        print(h(bytes(out)))
    elif mode == "copy_file_range":  # copy to FILE.copy on the same fs, hash the copy
        src = os.open(path, os.O_RDONLY)
        dst = os.open(path + ".copy", os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
        size = os.fstat(src).st_size
        left = size
        while left > 0:
            n = os.copy_file_range(src, dst, left)
            if n == 0:
                break
            left -= n
        os.close(dst)
        with open(path + ".copy", "rb") as f:
            print(h(f.read()))
    elif mode == "odirect":
        fd = os.open(path, os.O_RDONLY | os.O_DIRECT)
        size = os.fstat(fd).st_size
        buf = mmap.mmap(-1, (size + 4095) // 4096 * 4096)
        n = os.readv(fd, [buf])
        print(h(buf[:min(n, size)]))
    else:
        print("unknown mode", file=sys.stderr)
        sys.exit(2)
except OSError as e:
    print(f"ERR {e.errno} {os.strerror(e.errno)}")
