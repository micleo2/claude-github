# Phase 0 findings: fanotify pre-content hydration

Run: `sudo ./run-tests.sh` (needs root and kernel ≥ 6.14; `FSTYPE=xfs` etc. to try other filesystems).
Measured on kernel 6.18.44, ext4 (this kernel has no xfs/btrfs built in, so those still need a run elsewhere).

## Access paths: all hydrate correctly (28/28)

`read`, `pread` (range only; the file stays sparse), `mmap` read and `MAP_SHARED` write, `pwrite` into a
never-read placeholder, append, `truncate` shrink/grow, `sendfile`, `splice`, `copy_file_range`, `O_DIRECT`,
`io_uring` `IORING_OP_READ`, `exec` of scripts and ELF binaries, `cp`, `mv` across filesystems.

Every access generates `FAN_PRE_ACCESS` with a range. mmap faults ask for the whole mapping
(`off=0 count=<mapping size>`). Writes and truncates also produce events, covering the page they touch.

## Things that shaped the design

1. **`SEEK_DATA`/`SEEK_HOLE` bypass hydration.** GNU `cp`, and anything else copying sparse files, calls
   `lseek(SEEK_DATA)` first. On a placeholder that returns `ENXIO`, meaning "all hole", so the copy is written as
   zeros **without a single read**. `lseek` emits no pre-content event. `FICLONE` (reflink) on btrfs/xfs is expected
   to behave the same way; not verified here.
   **Mitigation:** also listen for `FAN_OPEN_PERM`, and hydrate the *whole* file at open when either:
   - it is ≤ `full-below` bytes (the default), or
   - the opener's executable is a known sparse-aware copier.

   Range-only hydration is an optimization reserved for large files opened by other programs.
2. **Creating a placeholder triggers events on the listener's own mount.** `ftruncate` on a fresh file emits
   `FAN_PRE_ACCESS` before its state xattr exists. The spike then marked it "not ours → ignore" and never hydrated it.
   **Mitigation:** the daemon creates placeholders through an **unmarked view** of the filesystem. The user-facing
   path is a bind mount, and only that mount carries the mark. The daemon's own I/O never generates events, and no
   self-deadlock is possible.
3. **Hydration state must be keyed by inode incarnation, not path.** Delete + recreate under the same name reused
   the stale map and served zeros. The spike keys by `(dev, ino, FS_IOC_GETVERSION)`. The real daemon keys by a file
   ID stored in an xattr and cross-checks it against the index.
4. **No listener ⇒ zeros.**
   - With no listener, placeholders read as zeros.
   - If the listener is SIGKILLed while an event is pending, the blocked reader is released and also reads zeros.

   Both confirmed. **Mitigation:** the user-facing bind mount exists only while the listener runs (see PLAN.md §4.6).
5. **Errors propagate.** `FAN_DENY | errno<<24` surfaces as that errno to the app (`EIO` tested).
6. **Overhead.** 2000 already-local small files, reading each with `cat`:
   - cold, first contact with the listener: ~280 ms (about 2 round trips per file: `OPEN_PERM` + `PRE_ACCESS`)
   - after an evictable ignore mark (`FAN_MARK_IGNORE_SURV|FAN_MARK_EVICTABLE`): ~29 ms
   - with no listener: ~30 ms

   So once a file is local, the steady-state cost is effectively zero.
7. Writing through the event fd (opened `FMODE_NONOTIFY`) generates no further events, but it does bump mtime.
   Restore it with `utimensat` after filling.

## Other filesystems and kernels (QEMU, `e2e/vm/run.sh`)

Ubuntu kernels 6.14.0-37 and 6.17.0-40 were booted under QEMU with separate ext4, xfs and btrfs disks.

**What passes on all three filesystems and both kernels:**
- listener start
- read, mid-file pread (the file stays sparse), mmap, `O_DIRECT`, io_uring, `cp`, exec of a script
- truncate, errno propagation, and "no listener reads zeros"
- the `lib/hsm` tests: lease-based eviction, concurrent openers, policy, view lifecycle

tether has no filesystem-specific code. The kernel decides which filesystems get pre-content events.

| Result | ext4 | xfs | btrfs |
|---|---|---|---|
| Access paths above | pass | pass | pass |
| Disk cost per placeholder (3 xattrs, measured with `df` over 2000) | ~4.1 KB (xattrs spill out of the 256 B inode into one block) | ~550 B (fit in the inode) | ~1.4 KB (metadata) |

On the host kernel (6.18), tmpfs, ramfs and overlayfs refuse pre-content marks with `EOPNOTSUPP`.

**Kernel difference found: exec'ing an ELF binary with range-only hydration.**
- On **6.14**, the page faults of an exec'd binary's mappings generate **no** pre-content event. The process runs on
  zero pages and segfaults (exit 139), on every filesystem.
- On **6.17** the same test passes.
- The exec open itself does raise `FAN_OPEN_PERM` on both kernels. tether hydrates whole files at open, so it is
  unaffected (the "full-*" checks pass on 6.14).
- A future range-hydration mode must therefore either require ≥ 6.17 or hydrate executables fully at open.

(The truncate failure that first showed up in the VM was a spike bug, not a kernel difference: the spike wrote a whole
64 KiB block past the new EOF. The events for `ftruncate` are identical on 6.14, 6.17 and 6.18.)
