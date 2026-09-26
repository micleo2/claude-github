# Evicting local content: survey and what it means for tether

Eviction turns a local file back into a placeholder to free space. It happens in two ways:

- **Explicitly:** `tether evict <path>`.
- **Automatically:** when the folder's cache budget is exceeded. Least recently used files go first, and prefetched
  files that nobody opened go before everything else.

This document covers how mature systems design eviction, and what tether should take from them.

## 1. What we measured (September 2026)

Setup: the V8 source tree (19,816 files, 186 MB) on the Arch client of the two-machine deployment. The data is on btrfs.

tether evicts each file in five steps:

1. **Index lookups:** our version equals the global version, and another device holds that exact version.
2. **Lease:** open the file and take a write lease, which proves nobody has it open and keeps other openers out.
3. **Re-hash:** re-read every block and compare it with the index.
4. **Placeholder:** mark the file and truncate it to a placeholder.
5. **Commit:** update the index to flag the file as virtual.

| | Result |
|---|---|
| Evict the whole tree (19,019 local files), one index transaction per file | **299 s** (twice), about 15 ms per file |
| Evict `src/heap` (333 files) | 9.4 ms per file; the daemon was on the CPU 11% of the time |
| Syscall profile while evicting `src/objects` (464 files, 5.2 s) | 21 `fsync` calls in total (0.15 s). Hashing and file I/O are negligible. The rest is threads waiting on each other (`futex`). |
| Index updates committed per 1000 files (commit ad8dc9e) | **2.2 s** for the whole tree, re-hash still included |

**Cause:** per-file bookkeeping, not I/O. Each index update is its own transaction and publishes its own events.
[small-file-hydration.md](small-file-hydration.md) found the same cost on the download side: a slow subscriber blocks
Syncthing's event bus for up to 15 ms. Batching removed both, as it did there.

**What batching gives up:** a crash between truncating a file and committing the batch leaves a placeholder that the
index still calls local. The scanner already repairs this case ("interrupted eviction" in `walker.walkPlaceholder`).
In the meantime, the precondition that another device holds the file, plus Syncthing's block hashes, keep any peer
from accepting the missing content.

## 2. What other systems do

None of the systems below re-reads and re-hashes content before evicting it. Each relies on a cheap state it already
tracks: an in-sync bit that the filesystem clears on any write, an "uploaded" state, a dirty flag, or a data-version
counter checked under a lease. They select candidates in bulk against high and low watermarks, skip files that are
open or dirty, and batch their bookkeeping.

| System | Trigger | Policy | Safety check before evicting | Open files | Scaling |
|---|---|---|---|---|---|
| **Windows Cloud Files API** (OneDrive, Nextcloud, SeaDrive, Dropbox) [1–7] | User "Free up space"; Storage Sense on low space or a schedule | Unused for N days (30 by default); pinned files exempt | In-sync bit that the filter clears on local writes; a USN guards the transition | Needs an exclusive handle; oplocks recommended | Asynchronous, driven by attribute changes; the platform may dehydrate by itself |
| **Apple File Provider** (iCloud, Dropbox on macOS) [8–10] | Disk pressure; `evictItem`; remote update (content policy) | The "minimum set" of least recently used files | Evicts only items reported as uploaded, never items with pending changes | `EBUSY` | Inside the system; not documented |
| **Linux cachefiles** (`cachefilesd`) [11–13] | Free-space and free-file watermarks | atime LRU, with `run`/`cull`/`stop` hysteresis (7%/5%/1%) | None needed: a read-only cache | Kernel "in use" mark; skipped | Bounded cull table (4096 by default), rescanned as space frees |
| **CernVM-FS** [14–15] | Quota exceeded | LRU by sequence number; cleans down to half the quota | None: content-addressed | Skipped first, evicted last | Buffered access updates, candidates in batches of 1000, one `DELETE`, background unlink, `synchronous=0`, rebuildable database |
| **rclone mount** [16–17] | Poll interval (1 min), quota, out-of-space kick | Maximum age, then least recently accessed | In-memory dirty flag | Never evicted | Periodic sweep; may exceed the quota between polls |
| **git-annex `drop`** [18–20] | User | Explicit | Contacts remotes to verify the required copy count (numcopies) and locks their content | n/a | `--jobs`, `--batch` |
| **Lustre HSM + RobinHood** [21–24] | Usage watermarks (e.g. 95%/93%, checked every 5 min) | LRU attribute; caps on count and volume per run | Archived, not dirty, and the **data version** matches the archive, checked under an exclusive-open lease | The lease breaks on any open; `EBUSY` | Worker threads, per-run caps, suspension on high error rates |
| **SeaDrive** [25–26] | Size limit, checked periodically | Modification time; cleans down to 70% of the limit | Not documented | Not documented | Periodic; users report it not keeping up |
| **EdenFS** [27–28] | Periodic | Last-access cutoff (inode unloading) | "Materialized" bit | n/a | Background |
| **Nydus, stargz** [29–31] | – | No bounded eviction (Nydus is adding reference-count GC) | – | – | – |

Notes on the closest analogues:

- **Lustre's `hsm_release` is nearly tether's design.** The client takes an exclusive-open lease, which any other open
  breaks, and releasing fails with `EBUSY` if the file is already open. At close, the metadata server checks that the
  lease was not broken and that the file's data version matches the archive before dropping the data [22–23]. It
  compares a counter, not a hash.
- **Windows' in-sync bit is maintained by the filter,** so any local write clears it without the provider's help.
  Dehydration fails with `ERROR_CLOUD_FILE_NOT_IN_SYNC` or `ERROR_CLOUD_FILE_PINNED` [1–3].
- **Git and Syncthing trust cached stat data** (mtime, ctime, size, inode, mode). Git re-reads content only for
  "racily clean" entries whose mtime is not older than the index file [32–33]. Since Linux 6.13, multigrain
  timestamps make ctime and mtime fine-grained once they have been queried, which shrinks that window [34]. The
  kernel's change counter (`i_version`, `STATX_CHANGE_COOKIE`) is not exposed to userspace [35].
- **CernVM-FS keeps LRU bookkeeping cheap** with a monotonically increasing access sequence number instead of atime,
  and treats its database as a rebuildable cache [15].

## 3. Recommendations for tether

Mapped to the five eviction steps above.

| # | Change | Why | Status |
|---|---|---|---|
| 1 | **Batch index updates** (step 5) | The per-file transaction and events were the whole cost | **Done** (ad8dc9e): 299 s → 2.2 s |
| 2 | **Replace the re-hash with a clean-state check under the lease** (step 3). Keep a full `--verify` mode. | Every other system compares a cheap state. Re-hashing costs a full read of every evicted byte: small for V8, a lot for large media. | **Done:** size and mtime, as the scanner decides that a file is unchanged; `tether -verify evict` / `?verify=true` re-reads every block. See below. |
| 3 | **Evict down to a low watermark** (80% of the budget) | Hysteresis as in cachefiles, CernVM-FS (50%), SeaDrive (70%) and RobinHood (95→93). Under a 30 MB budget, the V8 stress test produced 249 small eviction batches in about a minute. | **Done:** 249 → 48 batches for the same three passes, with no read errors. No per-run caps yet. |
| 4 | **Track recency with an access sequence number** in the index instead of filesystem atime | atime depends on `relatime`/`noatime`, and prefetched files are marked with an artificial atime today | **Not applicable as designed:** see below. |
| 5 | **Select candidates in one pass** (steps 1 and 2): one query per subtree, and skip busy files instead of failing | CernVM-FS, Apple, rclone and cachefiles all skip busy files and continue | Partly: bulk eviction already skips failures and continues |
| 6 | **Keep treating "another device holds this version" as recorded state,** not a live query per file | Apple ("reported as uploaded") and RobinHood (`synchro`) do the same. git-annex's live checks are for a stronger guarantee than a sync hub needs. | As today |

Notes on 2 and 4:

- **2, the clean-state check.**
  - **What it compares:** the index records no ctime, so the check compares size and mtime, which is the scanner's own
    definition of unchanged.
  - **Its limit:** a write that keeps both is also invisible to syncing, which would never upload it. Eviction without
    `-verify` can drop such a change.
  - **With `-verify`:** the block re-read catches it, refuses the eviction, and schedules the rescan that makes the
    change visible (e2e `evict_verify_catches_invisible_change`).
  - **Measured on V8:** evicting the whole tree took 1.96 s (2.2 s with re-hashing). Its small files are already in
    the page cache, so eviction was mostly bookkeeping. The gain grows with large files and slow disks.
  - **Stricter, later:** record the ctime after hydration and after each scan.
- **4, an access sequence number.** CernVM-FS can keep one because, as a FUSE filesystem, it sees every access.
  tether sees accesses only while a file is a placeholder: once it is local its mark is removed, so later reads raise
  no events. Counting them would need an access mark on every local file, which is an event per read. Recency
  therefore stays atime-based. Under `relatime` that resolves to about a day for files read repeatedly, and under
  `noatime` it degrades to download order.

Not recommended: asynchronous truncation or unlinking. In CernVM-FS it frees space off the critical path, but for
tether the truncation is what makes the placeholder and must happen under the lease.

## Sources

The survey agent read most primary sources directly. It could not verify:

- Apple's and OneDrive's internal batching and thresholds;
- Dropbox's exact inactivity period;
- git-annex drop throughput;
- the EdenFS default unload age in source.

1. https://learn.microsoft.com/en-us/windows/win32/api/cfapi/nf-cfapi-cfupdateplaceholder
2. https://learn.microsoft.com/en-us/windows/win32/api/cfapi/nf-cfapi-cfsetinsyncstate
3. https://learn.microsoft.com/en-us/windows/win32/api/cfapi/ne-cfapi-cf_update_flags
4. https://learn.microsoft.com/en-us/previous-versions/mt827480(v=vs.85)
5. https://learn.microsoft.com/en-us/windows/win32/api/cfapi/ne-cfapi-cf_hydration_policy_modifier
6. https://support.microsoft.com/en-us/office/use-onedrive-and-storage-sense-in-windows-10-to-manage-disk-space-de5faa9a-6108-4be1-87a6-d90688d08a48
7. https://github.com/nextcloud/desktop/blob/master/src/libsync/vfs/cfapi/cfapiwrapper.cpp
8. https://developer.apple.com/videos/play/wwdc2021/10182/
9. https://developer.apple.com/documentation/fileprovider/synchronizing-the-file-provider-extension
10. https://developer.apple.com/documentation/fileprovider/nsfileprovidermanager/evictitem(identifier:completionhandler:)
11. https://www.kernel.org/doc/html/latest/filesystems/caching/cachefiles.html
12. https://manpages.debian.org/testing/cachefilesd/cachefilesd.conf.5.en.html
13. https://github.com/torvalds/linux/blob/master/fs/cachefiles/namei.c
14. https://cvmfs.readthedocs.io/en/2.9/cpt-details.html
15. https://github.com/cvmfs/cvmfs/blob/devel/cvmfs/quota_posix.cc
16. https://rclone.org/commands/rclone_mount/
17. https://github.com/rclone/rclone/blob/master/vfs/vfscache/cache.go
18. https://git-annex.branchable.com/copies/
19. https://git-annex.branchable.com/git-annex-drop/
20. https://git-annex.branchable.com/devblog/day_322-326__concurrent_drop_safety/
21. https://wiki.old.lustre.org/index.php/Architecture_-_HSM_Migration
22. https://lkml.rescloud.iu.edu/1311.2/01248.html
23. https://github.com/cea-hpc/robinhood/wiki/v3_lhsm_tuto
24. https://github.com/cea-hpc/robinhood/wiki/robinhood_v3_admin_doc
25. https://help.seafile.com/drive_client/drive_client_for_win10/
26. https://github.com/haiwen/seafile-client/issues/1082
27. https://github.com/facebook/sapling/blob/main/eden/fs/docs/Inodes.md
28. https://github.com/facebookexperimental/eden/blob/main/eden/fs/service/EdenServer.cpp
29. https://github.com/dragonflyoss/nydus/blob/master/docs/nydus-design.md
30. https://github.com/dragonflyoss/nydus/pull/1894
31. https://github.com/containerd/stargz-snapshotter/issues/2395
32. https://git-scm.com/docs/racy-git
33. https://docs.syncthing.net/users/syncing.html
34. https://docs.kernel.org/filesystems/multigrain-ts.html
35. https://github.com/torvalds/linux/blob/master/fs/stat.c
