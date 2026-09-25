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
