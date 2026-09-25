# Plan: Linux-native selective sync (Dropbox-style "online-only" files, plain files everywhere)

Working name: **`tether`** (placeholder, rename freely).

## 1. What we are building

A Dropbox-like sync tool, Linux server + Linux clients, with:

- **Selective sync with placeholders.** Every client sees the *whole* tree (names, sizes, mtimes, permissions), but file
  *content* is local only when pinned or recently used. Opening an "online-only" file transparently downloads it.
- **Plain files on every machine.** The server's store is an ordinary directory tree you can `ls`, `rsync`, back up, serve
  over Samba, or walk away from. A client's sync root is an ordinary directory on ext4/xfs/btrfs. There is no block store,
  no content-addressed object DB, and no custom container format anywhere.
- **Linux only, on purpose.** We use kernel APIs that cross-platform tools can't rely on: fanotify pre-content (HSM)
  events, filesystem-wide fanotify change notification, reflinks, hole punching, `renameat2`, `O_TMPFILE`, and btrfs
  snapshots.

## 2. Summary of prior art, and what we take from each

| | Syncthing | Seafile (+ SeaDrive) | **tether** |
|---|---|---|---|
| Storage on server | Plain files | Custom object store (commits, fs objects, CDC blocks) | **Plain files** |
| Storage on client | Plain files | Plain files (sync client), or FUSE cache (SeaDrive) | **Plain files; unfetched ones are sparse placeholders** |
| Topology | P2P mesh, symmetric | Client/server | **Hub: one authoritative server, many clients (on the Syncthing protocol)** |
| Selective sync | Ignore patterns only (a file is either fully there or invisible) | Per-library/sub-folder sync; SeaDrive gives on-demand files through FUSE | **Three states per path: ignored / online-only / local, plus an LRU cache budget** |
| On-demand mechanism | — | FUSE (every I/O goes through userspace) | **fanotify pre-content events (kernel ≥ 6.14 required)** |
| Change detection | inotify per directory, plus periodic full scans | inotify | **fanotify filesystem/mount marks (no per-directory watch limit)** |
| History/versioning | Per-folder versioning into `.stversions` | Built into the object model | **btrfs/ZFS snapshots on the server, plus Syncthing-style versioning as a fallback** |
| Dedup | None | Block-level via CDC | **Reflinks / offline dedup (duperemove) on the server filesystem** |

**The key point:** Seafile's custom format exists mainly for three things: history, dedup, and cheap snapshots.
On a Linux server those come from the **filesystem** (btrfs/ZFS/XFS reflinks), so the format isn't needed.
What Seafile has that Syncthing lacks is **on-demand files** (SeaDrive). SeaDrive does this with FUSE; we can do it
with kernel HSM hooks on a real filesystem instead.

## 3. Recommendation: fork Syncthing and add a "virtual folder" mode backed by fanotify HSM

### Why fork Syncthing rather than start fresh or start from Seafile

Syncthing already has most of the hard parts, and they fit this design:

1. **The client already receives the complete global index.** Every file's name, size, mtime, permissions, version
   vector, and **block list with SHA-256 per block** comes down even for files the client never downloads. That is
   exactly the metadata a placeholder needs.
2. **BEP `Request` messages are already ranged by block** (folder, name, offset, size, hash). On-demand partial
   hydration ("give me the blocks covering bytes 4 MiB–6 MiB") needs no protocol change.
3. It has mature conflict handling, version vectors, TLS device identity, relays/NAT traversal, a GUI, and a REST API.
   Since v2.0 the index lives in SQLite, which is easier to extend with new state columns than the old LevelDB schema.
4. "Hub" topology is a deployment choice in Syncthing: clients connect only to the server. We add config presets for it
   rather than rewriting the model.

Seafile is the wrong base because its whole server is built around the object store we want to drop. We take **design
lessons** from it (Section 7), not code.

### Why fanotify HSM rather than FUSE (the SeaDrive/rclone approach)

Linux 6.14 merged **fanotify pre-content events** (`FAN_CLASS_PRE_CONTENT` + `FAN_PRE_ACCESS`). This is the Linux
equivalent of the Windows Cloud Files API used by OneDrive and Dropbox. Meta built it and runs it in production. A
privileged listener gets a *blocking* event carrying the **byte range** about to be accessed, fills that range, and then
allows the access.

- Hydrated files are ordinary files on ext4/xfs/btrfs, read at native speed with no userspace in the I/O path.
- POSIX semantics (locks, mmap, `O_DIRECT`, `splice`, `copy_file_range`, `exec`) come from the real kernel
  filesystem. We don't have to reimplement them in a FUSE server.
- If the sync daemon crashes, hydrated files stay fully usable. A FUSE mount goes `ENOTCONN` instead.
- Evicting a file is `fallocate(FALLOC_FL_PUNCH_HOLE)`, and the file stays in place.

Prior art proving it works: [franzjeger/HydrationAPI](https://github.com/franzjeger/HydrationAPI) implements a
OneDrive client this way. It passes its invariants on btrfs, ext4, and xfs. Its findings are in Section 4.6.

**No fallback.** Clients must run a kernel with fanotify pre-content support (≥ 6.14) on ext4, xfs or btrfs. On
anything else, an on-demand folder refuses to start (it reports a folder error) rather than degrading. A plain
send-receive folder still works there, but it downloads everything.

## 4. Architecture

```
                        ┌─────────────────────── server (Linux) ───────────────────────┐
                        │ tether (Syncthing fork), folder type = send-receive          │
                        │ store: /srv/tether/<user>/<folder>  ← plain files on btrfs   │
                        │ fanotify FS-wide change watcher; btrfs snapshots for history │
                        └──────────────────────────────▲───────────────────────────────┘
                                                       │ BEP over TLS (unchanged wire protocol + small extensions)
┌────────────────────────────── client (Linux) ────────┴───────────────────────────────────────────────┐
│  tether (user service, unprivileged)                    tether-hsmd (system service, CAP_SYS_ADMIN)  │
│  ├─ model/puller/scanner (Syncthing)                    ├─ fanotify group FAN_CLASS_PRE_CONTENT       │
│  ├─ NEW: folder type "virtual"                          ├─ mark on the sync-root mount (FAN_MARK_MOUNT)│
│  ├─ NEW: hydration service  ◄──── unix socket ────────► ├─ gets FAN_PRE_ACCESS{fd, range, pid}       │
│  │     (fetch blocks, verify SHA-256, write to fd)      ├─ passes the event fd via SCM_RIGHTS         │
│  ├─ NEW: pin/evict/cache-budget manager                 ├─ replies FAN_ALLOW / FAN_DENY(errno)        │
│  └─ D-Bus + CLI + REST                                  └─ FS-wide change events → forwarded to daemon │
│                                                                                                      │
│  ~/Tether  ← bind mount of /var/lib/tether/<user>/root (ext4/xfs/btrfs), only present while hsmd runs │
└──────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

### 4.1 Per-path states on the client

| State | On disk | In index | Advertised to peers as "have" |
|---|---|---|---|
| **Ignored** | absent | Syncthing's `FlagLocalIgnored` | no |
| **Online-only** (dehydrated) | sparse file with correct size/mtime/mode; xattr `user.tether.state=virtual` | NEW `FlagLocalVirtual` | **no.** We must never claim blocks we don't have |
| **Partial** | some ranges filled; bitmap of filled blocks in DB | `FlagLocalVirtual` + block bitmap | no (the hub is the block source) |
| **Local** (hydrated, unpinned) | full content | normal | yes |
| **Pinned** | full content, never evicted | normal + pin flag | yes |

Pin policy comes from rules that inherit down directories, like `.stignore` patterns (`.tetherpin`, or config).
`tether pin/unpin/status/evict <path>` and the D-Bus API write those rules.

### 4.2 Hydration path (reading an online-only file)

1. An app calls `read()`, `mmap()`, and so on on `~/Tether/a/b.mkv`. The kernel emits `FAN_PRE_ACCESS` with the range
   and blocks the app.
2. `hsmd` checks that the fd's inode belongs to a registered sync root and user. It forwards `(fsid, ino, range, pid)`
   and the event fd (via `SCM_RIGHTS`) to that user's `tether` daemon.
3. The daemon maps the inode to an index entry (inode→path table, cross-checked with an xattr file ID). It works out
   which blocks cover the range, **widened by a readahead policy**: whole file below N MiB, sequential-read detection,
   and so on. It sends BEP `Request`s to the hub, verifies each block's SHA-256, and `pwrite`s into the event fd.
   The fd is opened `FMODE_NONOTIFY`, so these writes don't re-trigger events.
4. The daemon restores mtime, updates the block bitmap, and tells `hsmd` "done". `hsmd` writes `FAN_ALLOW`.
5. Failure cases:
   - Offline or timeout → `FAN_DENY` with `EIO`/`EAGAIN` (errno-carrying denials were added with the HSM work).
   - A blocked process (see 4.5) → `FAN_DENY`.
   - The daemon dies mid-request → `hsmd` answers `EIO` itself.

### 4.3 Writes, renames, deletes on placeholders

- **Write to a virtual file:** v1 policy is to hydrate the *whole* file before allowing write access. Once full, it is a
  normal file, and Syncthing's scanner handles it as usual. Ranged write-hydration is a later optimization.
- **Rename/move of a placeholder:** Syncthing detects renames by hashing content, which would hydrate the file. Instead,
  the scanner **never reads virtual files**. It identifies them by inode plus the xattr `user.tether.fileid` (a hash of
  the global name+version). A placeholder that shows up under a new name with the same file ID becomes a rename. We send
  a new index entry reusing the block list, with no data transfer.
- **Atomic-save editors** (write temp → `rename` over the original) drop xattrs. That's fine: the result is a fully
  local new file. But the scanner must not treat "xattr gone" as "file is virtual/empty". The DB is the source of truth;
  xattrs are hints for file managers.
- **Delete:** a normal Syncthing deletion. A missing file is a positive delete, never "maybe not hydrated".
- **Truncate/`O_TRUNC` open:** handled the same way as a write.

### 4.4 Eviction and cache budget

- Config: `cacheBudget` (bytes or % of FS), `minFree`, per-folder policy (`online-only-by-default` | `local-by-default`).
- LRU by last hydration/access time. Access times come from cheap non-permission `FAN_ACCESS`/`FAN_OPEN` events on the
  same mark, not `atime`.
- Only evict files that are fully synced (local version == global version), not pinned, and **not open**:
  1. Take an `F_SETLEASE` write lease in `hsmd`. That fails if anyone has the file open.
  2. Punch the hole.
  3. Set the xattr.
  4. Release the lease.
- `evict` is also a user command ("free up space" in the file manager).

### 4.5 Protecting against accidental mass hydration

This is the classic HSM problem: indexers, thumbnailers, `updatedb`, backup tools, `grep -r`, and antivirus touch every
file.

- The event carries the `pid`. Apply a per-user **process policy**: `/proc/<pid>/exe` → allow / deny-with-`EAGAIN`.
  Ship defaults for `tracker-miner`, `baloo`, `updatedb`/`plocate`, thumbnailers, and `rsync`/`borg`/`restic`
  (configurable).
- Ship an `updatedb.conf` `PRUNEPATHS` drop-in and Tracker/Baloo exclusion config for the sync root.
- Add a "hydration storm" breaker: if more than X MiB/s is demanded by a single non-interactive process, pause and
  notify via D-Bus.

### 4.6 Safety invariants (lessons from HydrationAPI and HSM design)

1. **Never expose sparse placeholders without a listener.** If no fanotify listener is attached, reads of a placeholder
   return **zeros**, silently. An app could then save those zeros back and we'd upload them. Mitigation:
   - The user-visible sync root is a **bind mount created by `hsmd` after the fanotify mark is in place**, and torn down
     when `hsmd` stops (systemd `BindsTo=`).
   - The underlying `/var/lib/tether/<user>/root` stays mode 0700 and root-owned at the top level, so users can't
     bypass the mount.

   This also satisfies the "sync root must be its own mount" constraint HydrationAPI found: fanotify can't usefully mark
   single directories for this.
2. **Supervisor–worker split.** `hsmd` (small and privileged) outlives user daemons and fails closed (`EIO`). The two
   dying at once remains a hole that only mitigation 1 closes.
3. **The event's `count` is a demand, not a hint.** The full requested range must be written before `FAN_ALLOW`, or the
   app reads zeros. mmap faults ask for the mapping size, and consecutive events can overlap.
4. **Upload on quiet, not on close.** Keep Syncthing's scanner debounce. Resolve names at send time; address files by
   inode.
5. **Never upload content from a file whose DB state is virtual/partial** unless the kernel shows a real modification.
   Check ctime and size change against the recorded placeholder stat. Ambiguous cases become conflict copies, never
   overwrites.

### 4.7 Other Linux-specific integration

| Need | API |
|---|---|
| Scalable change detection (client & server), no `max_user_watches` limit | fanotify `FAN_MARK_FILESYSTEM`/`FAN_MARK_MOUNT` + `FAN_REPORT_DFID_NAME` (replaces Syncthing's inotify watcher; inotify stays as the unprivileged fallback) |
| Atomic creation / replace of downloaded files | `O_TMPFILE` + `linkat`, `renameat2(RENAME_NOREPLACE / RENAME_EXCHANGE)` |
| Cheap conflict copies & versioning | `ioctl(FICLONE)` / `copy_file_range` (reflink on btrfs/xfs) |
| Placeholder creation without allocating space | `ftruncate` to size (sparse), `fallocate(PUNCH_HOLE)` to evict |
| Server history | btrfs subvolume snapshots on a schedule, or per sync batch, exposed as "previous versions" via REST/CLI; ZFS equivalent |
| Server dedup | reflinks on rename/copy detection; optional `duperemove` |
| Throughput for scanning/hashing | batched `statx`; later `io_uring` for hashing and block serving |
| Desktop | D-Bus service (`org.tether.Sync1`), `org.freedesktop.Notifications`, Nautilus (`nautilus-python`) and Dolphin overlay plugins that read state via D-Bus / xattr |

## 5. Syncthing changes, concretely

Fork from the latest **v2.x** tag. Keep the changes in new packages so we can keep merging upstream:

- `lib/config`: new folder type `virtual` (client), `hub` presets (disable global discovery/relays optionally, one
  trusted introducer = server). New options: `onDemand`, `onDemandView`, `cacheBudget`, `pinPatterns`.
- `lib/db` (SQLite): `FlagLocalVirtual`, per-file block bitmap table, pin table, inode↔file ID table, LRU table.
  **Index-sending code must never announce virtual/partial files as "have".** The pattern is how `FlagLocalReceiveOnly`
  and `FlagLocalIgnored` are filtered today.
- `lib/model/folder_virtual.go`: a new folder implementation (sibling of `folder_sendrecv.go`).
  - Its puller creates/updates **placeholders** (metadata only) for new or changed global files that aren't pinned.
  - It pulls fully for pinned ones.
  - When a remote change arrives for a *hydrated* unpinned file, it either re-hydrates (the file is "hot") or demotes it
    to a placeholder (policy).
- `lib/scanner`: skips hashing virtual files. Detects renames of virtual files via file ID. Detects "was virtual, now
  locally written" transitions.
- `lib/hsm` (new): the fanotify pre-content listener plus placeholder helpers (mark, hydrate, evict).
- `lib/fs`: fanotify-based watcher (`lib/fs/fanotify_linux.go`) beside the existing notify-based one.
- `cmd/tether-hsmd` (new): the small privileged helper. Go with `golang.org/x/sys/unix` (add the missing pre-content
  constants and structs), or Rust if we want the privileged part minimal and memory-safe. **Decision: Go** for one
  toolchain, and keep it under ~2k lines.
- GUI: per-file state column, pin toggle, cache usage bar.

The wire protocol (BEP) stays compatible. A stock Syncthing server can serve a tether client, which is useful for
testing and adoption. Later optional extensions (for example a batched `RequestRange`, and server-pushed "prefetch
hints") are negotiated via a hello option.

## 6. Phased roadmap

| Phase | Goal | Exit criteria |
|---|---|---|
| **0. Spikes (2–3 wks)** | Validate the kernel mechanism before touching Syncthing | A C/Go prototype on 6.14+ over ext4, xfs, btrfs hydrates on `read`, `mmap` (read + write fault), `exec`, `sendfile`, `splice`, `copy_file_range`, `io_uring` read, `O_DIRECT`. Measure the first-byte latency added. Confirm listener-absent behaviour, `FAN_DENY` errno, lease-based eviction, and range semantics on overlapping events. Read HydrationAPI's conformance tests and SeaDrive's cache logic |
| **1. Fork + hub mode** | Rename/branding and config presets | Client↔server sync of plain files with fanotify watcher on both ends; 1M-file tree scans without inotify limits |
| **2. Virtual folder, metadata only** | Placeholders appear; nothing hydrates yet | Whole tree visible with correct `stat`; zero bytes allocated; server index never corrupted; stock Syncthing peer interop test |
| **3. hsmd + on-demand hydration** | Open file → content appears | All Phase 0 access paths pass; block hash verification; offline → `EIO`; daemon kill → fail closed; bind-mount guard |
| **4. Pin / evict / budget** | Dropbox "Make available offline / Online-only" | CLI + REST + D-Bus; LRU eviction under budget; lease-safe eviction; process policy + storm breaker |
| **5. Local edits on virtual files** | Writes, renames, deletes, atomic-save editors | Rename of placeholder = zero transfer; conflict copies via reflink; fault-injection suite (kill -9 at every step) finds no data loss |
| **6. Desktop integration** | File-manager emblems, context menu, notifications | Nautilus + Dolphin plugins, progress notifications for large hydrations |
| **7. Server features** | History and multi-user | btrfs snapshot versions browsable/restorable; per-user folders owned by the matching Unix user |
| **8. Hardening** | Ready for real data | Crash-consistency tests, fuzzing of the hsmd socket protocol, 1M-file / 1 TB benchmarks, upgrade/rollback of the DB schema |

## 7. Research items from Seafile / SeaDrive

Read (not copy) these for design lessons:

- **SeaDrive cache manager:** eviction heuristics, how it avoids evicting files being edited, cache-size accounting,
  behaviour when the cache is full mid-download.
- **SeaDrive "file locking"** and how Office-style lock files are handled. Consider an optional server-side advisory
  lock via BEP extension in Phase 7.
- **Seafile's "sync sub-folder" / per-library selective sync UX:** what users actually toggle.
- **Seafile's FastCDC chunking:** worth considering later as an *index-only* change (content-defined block boundaries in
  the block list for better delta transfer on inserted bytes). Files stay plain. This would be a BEP extension, so defer.
- **Seafile's server-side history UX:** map it onto btrfs snapshots.

## 8. Requirements and constraints

- **Client kernel ≥ 6.14** for fanotify HSM mode, on ext4/xfs/btrfs. That means Fedora 42+, Ubuntu 25.04+/26.04 LTS,
  Arch, and similar. Older kernels (e.g. Debian 13, 6.12) are not supported for on-demand folders.
- `hsmd` needs `CAP_SYS_ADMIN`, because pre-content fanotify groups and mount marks aren't available unprivileged.
  Ship it as a systemd system service with a strict sandbox:
  - `CapabilityBoundingSet=CAP_SYS_ADMIN CAP_LEASE CAP_DAC_READ_SEARCH`
  - `ProtectSystem=strict`
  - no network access
- Per-user daemons stay unprivileged and hold all credentials and network access.
- Server: any Linux filesystem works. btrfs/ZFS/XFS are recommended for snapshots and reflinks.

## 9. Risks and open questions

1. **Kernel details still to verify in Phase 0:**
   - page-fault (mmap) pre-content coverage on each target filesystem (patches added fault hooks for some filesystems
     specifically)
   - which filesystems opt in to HSM events
   - `io_uring` coverage
   - the exact set of errnos allowed in `FAN_DENY`

   The design depends on these, which is why Phase 0 comes first.
2. **Lazy directory listing.** Pre-content events cover file *content*, not `readdir`/`lookup`, so v1 materializes the
   full tree of placeholders. Huge trees cost inodes but no data blocks. Subtrees users never want remain "ignored".
   If a lookup/readdir pre-content event lands upstream, adopt it.
3. **Placeholder disk usage.** Some filesystems allocate for sparse files or inline data. Measure it (HydrationAPI found
   a bug here).
4. **Both processes dying.** The bind-mount guard mitigates it; test it explicitly.
5. **Upstream drift.** Keep the fork's diff in `lib/model` and `lib/db` small, and rebase on Syncthing releases monthly.
6. **Multi-user server.** Syncthing has no concept of user accounts. v1 = one server instance per user (systemd template
   unit `tether@user`). A true multi-tenant server is a later project.

## 10. Immediate next steps

1. Write the Phase 0 fanotify HSM prototype (`spikes/fanotify-hsm/`) and its conformance test matrix.
2. Fork `syncthing/syncthing` at the latest v2 tag into this org. Set up CI with a VM-based test runner on a 6.14+
   kernel (fanotify HSM can't run in unprivileged containers).
3. Write the SQLite schema additions and the `FlagLocalVirtual` index-filtering change first. They are the safety-critical
   core: we must never advertise blocks we don't have.
