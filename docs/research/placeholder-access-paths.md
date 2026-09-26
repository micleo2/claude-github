# Placeholders read from other mount namespaces: survey, spikes and options

**Status:** option 1 (per-placeholder inode marks) and option 6 (the monitor holds the fanotify group across sync
process restarts) are implemented; see §8. The guards for a stopped service (options 4 and 5) are not yet.

Problem: tether marks one mount, the view, with a fanotify mount mark. A mount mark belongs to that one mount. Any copy of
it in another mount namespace has no mark, and a placeholder read through that copy returns zeros:

- **Readers:** `docker -v`, bwrap (what flatpak uses), `unshare -m`, and systemd services with their own mount namespace
  all get such a copy.
- **Data loss:** a sandboxed editor that saves a file it read as zeros would sync the zeros to every other device.

This breaks the rule that a placeholder is never readable as zeros.

## 1. What we found (September 2026)

Setup: the V8 source tree (19,816 files, 186 MB) on a Debian 13 hub, and an on-demand client on Arch (Linux 7.2, btrfs
`/home`). Each sandbox read the first 24 bytes of an online-only header:

| Reader | Result |
|---|---|
| The view, from the host namespace | Real content (downloaded on open) |
| `docker run -v ~/Documents/tether-test:/x` | Zeros, nothing downloaded |
| `bwrap --ro-bind / /` | Zeros, nothing downloaded |
| `unshare -Urm` | Zeros, nothing downloaded |

The server had `// Copyright 20…` for all three files.

**Making the view `MS_UNBINDABLE` does not help.** Docker and bwrap start from a full copy of the mount tree, which
includes unbindable mounts.

## 2. What other systems do

Every product that handles this safely keeps the "not here yet" state somewhere that no access path can go around: a
volume filter, an inode flag, or being the filesystem itself.

| System | Mechanism | Daemon not running | All access paths covered? |
|---|---|---|---|
| **Windows Cloud Files API** (OneDrive, Dropbox, Nextcloud, SeaDrive on Windows) [1–4] | `cldflt.sys` minifilter on the volume; NTFS reparse point; `FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS` | Fails: `ERROR_CLOUD_FILE_PROVIDER_NOT_RUNNING` (0x8007016A) | Yes above the filter. Microsoft warns that reads from below it "might return 0s" [2], the same class of hole as ours. |
| **Apple File Provider** (iCloud, Dropbox, Google Drive on macOS) [5–7] | APFS `SF_DATALESS` inode flag and a kernel upcall to `fileproviderd`; per-process opt-out via `IOPOL_TYPE_VFS_MATERIALIZE_DATALESS_FILES` | Fails: `EDEADLK` / `ETIMEDOUT` | Yes, because the state is on the inode. APFS snapshot reads don't download; what they return is **unverified**. |
| **Dropbox on macOS, 2016–2022** [8–9] | Kext with Kauth vnode listeners; FUSE rejected for context-switch cost | – | Replaced by File Provider after Apple deprecated kexts |
| **Linux FUSE clients**: SeaDrive, rclone, pCloud, onedriver [10–13] | Be the filesystem | Fails: `ENOTCONN` | Yes; every bind copy still reaches the daemon |
| **Nextcloud Linux suffix mode**, Resilio `.rsls` [14–15] | Placeholder has a different name (`x.nextcloud`) | Fails by construction | Yes, but apps can't open these files transparently. Nextcloud's reason: "For Linux … there is just no API available" [14] |
| **OpenCloud openVFS**, merged into Nextcloud desktop in September 2026 [16–17] | FUSE layer over a directory, state in xattrs | Unmounted. The survey agent read the source as exposing 0-byte files underneath (**unverified**) | Only through the mount |
| **Meta EdenFS** [18–20] | FUSE (Linux), NFSv3 (macOS), ProjFS (Windows) | Linux: `ENOTCONN`. "Takeover" hands the FUSE connection to a new daemon on restart. ProjFS: hydrated files stay readable, so every start runs fsck. | Yes on Linux (being the filesystem) |
| **VFS for Git** and GitHub's libprojfs [21–23] | ProjFS on Windows. The macOS kext died with Kauth. The Linux port became a stacked FUSE filesystem over a hidden directory, was "not especially performant", and is archived. | – | Microsoft moved to Scalar with sparse checkout, which avoids virtualising at all |
| **CernVM-FS** [24–25] | FUSE; containers bind it with `:shared` | `ENOTCONN` until cleaned up; CERN runs a sidecar to repair severed mounts | Yes |
| **Container lazy pulling**: stargz, SOCI, Nydus [26–28] | FUSE; Nydus also used erofs over fscache in-kernel | FUSE daemon restarts left containers with `ESTALE`/`ENOTCONN` [26]. fscache mode failed closed (EIO) and supported fd failover [28]. | Yes |
| **Lustre HSM** [29] | "Released" files have no data objects; the metadata server coordinates restores | Blocks, then times out, or returns `ENODATA` | Yes (filesystem level) |
| **Dropbox, Google Drive, OneDrive, Box, Synology on Linux** [30–31] | No online-only files on Linux | – | – |

## 3. The Linux kernel picture

- **Upstream is investing in fanotify pre-content for exactly this job.**
  - Linux 7.2 includes "erofs: remove fscache backend entirely" (verified in torvalds/linux). The stated replacement is
    erofs file-backed mounts plus fanotify pre-content hooks, and Nydus plans to move to them [32–33].
  - Meta runs pre-content hooks in production for on-demand package and executable fetching. It chose fanotify over
    FUSE because "If the FUSE daemon crashes, suddenly applications start crashing in production" [34–35].
- **The intended marking model has layers** [36]:
  - a mount or filesystem mark carries the permission events;
  - evictable inode ignore marks silence files that are already populated;
  - "persistent inode marks" in an xattr are listed as future work.

  The wiki's reference design, HTTPDirFS, is tether's design: mark a bind mount, then move it into place. **We found no
  upstream discussion of mount-namespace copies dropping the mark.** Amir Goldstein's 2021 "filesystem view mark" RFC is
  the closest, and it never landed [37].
- **Josef Bacik's `remote-fetch` proof of concept uses per-file inode marks** with `FAN_UNLIMITED_MARKS`, and removes
  each mark once the file is populated [38]. An inode mark is attached to the inode, not to a mount, so every path
  reaches it. Only marked inodes send events to the daemon. Every other open on that filesystem pays one extra in-kernel
  check [39].
- **What happens when the daemon dies:**
  - **Today it fails open.** Closing the group allows all pending events and drops every mark [40].
  - **Restartable permission events** (`FAN_CONTROL_FD`) would let a supervisor keep the group and its marks alive while
    the daemon restarts. The series was unmerged as of July 2026 [41–42].
  - There is no accepted proposal for an on-disk flag that fails closed; ownership of persistent marks is unresolved
    [35].
- **Feature status as of June 2026** [41]:
  - the page-fault (mmap) pre-content hook was reverted, so files must be complete by `mmap()` time (tether already
    hydrates at open);
  - directory lookup/readdir events are stuck on deadlocks.
- **FUSE passthrough** (6.9+) sends read, write and mmap straight to a backing file. Lookup, getattr and readdir still go
  to the daemon; getattr/readdir passthrough is in review. Setting up passthrough needs `CAP_SYS_ADMIN` [43–44]. Android
  has used FUSE plus passthrough for shared storage since Android 12 [45].
- **BPF LSM.** A `file_open` program can return `-EPERM` and read `user.*` and `security.bpf.*` xattrs [46–47]. It is
  active by default on Arch and Fedora, and not on Ubuntu [48–50]. On the two machines tested here, `bpf` is in
  `/sys/kernel/security/lsm` on both Arch (7.2) **and Debian 13** (6.12). The survey found no prior art for using it as
  an HSM guard.

## 4. Spikes on Linux 7.2

Code and runner: [`spikes/placeholder-marks/`](../../spikes/placeholder-marks/) (`run.sh coverage`, `run.sh markbench`).
The runs used loop-mounted filesystems in a privileged container on the Arch client (32 cores, NVMe).

### Coverage (`coverage`)

In both modes the daemon does its own I/O through a detached `open_tree()` clone that carries a
`FAN_MARK_MOUNT|FAN_MARK_IGNORE_SURV` ignore mark. The clone is never attached to any path.

| Check | Inode marks | Filesystem mark |
|---|---|---|
| Daemon's own reads through the detached mount | 0 events | 0 events |
| `unshare -m` (full copy of the mount tree) | downloaded | downloaded |
| Bind mount made after marking | downloaded | downloaded |
| `unshare` + bind inside it (what container runtimes do) | downloaded | downloaded |
| Ordinary path | downloaded | downloaded |
| Downloaded file re-read | 0 events | 0 events |
| 1,000 reads of a normal file | **0 events** | 1 event |

The survey left one question open: does a mount ignore mark also silence inode-mark events? It does (first row).

### Startup cost of per-placeholder inode marks (`markbench`)

Marks disappear with the fanotify group, so a restarted daemon must mark every placeholder again. The benchmark trees
have about 100 files per directory. Cold runs dropped the page cache first. "Held" is the growth in kernel slab memory
while the marks exist.

| Placeholders | ext4 cold / warm | btrfs cold / warm | Held |
|---|---|---|---|
| 20,000 | 33 ms / 11 ms | 45 ms / 9 ms | ~15–26 MB |
| 200,000 | 0.31 s / 0.14 s | 0.44 s / 0.17 s | ~200–250 MB |
| 1,000,000 | 1.5 s / 0.7 s | 2.2 s / 0.9 s | ~1.2–1.4 GB |

- **Time:** marking costs 10–15% more than an `lstat` of every placeholder. On a spinning disk the cold runs would be
  seek-bound, like any tree walk.
- **Memory:** the mark itself is about 120 B. A marked inode can't be evicted, so its cached inode and dentry (about
  1.2 KB) are held too. **Total: about 1.3 KB per online-only file**, not reclaimable while marked.
- **The real cost of marks vanishing is the gap, not the re-marking.** Between daemon death and the end of re-marking,
  placeholders read as zeros from every path. That gap needs its own answer (options 4 and 5 below).

## 5. Options for tether

| # | Option | Covers namespaces | Daemon down | Cost |
|---|---|---|---|---|
| 1 | **Per-placeholder inode marks** (+ detached ignored mount for the daemon) | Yes (spike) | Fails open once marks vanish; needs 4 or 5 | ~1.3 KB of kernel memory per online-only file; re-marking at startup (above); placeholders must be created under a temp name, marked, then renamed into place |
| 2 | **Filesystem mark** on a filesystem dedicated to tether | Yes (spike) | Fails open once the mark vanishes | Needs a partition, LV or loop image. On a shared filesystem, every open on it would go to the daemon. |
| 3 | **FUSE with passthrough** | Yes, by construction | Fails closed: `ENOTCONN` for the whole folder, including local files | Lookup/getattr/readdir go through the daemon; restart needs fd handoff (EdenFS, CernVM-FS, Nydus); gives up "plain files on a plain filesystem" |
| 4 | **Guard: pinned BPF LSM program** that denies opening placeholder-tagged inodes except through the daemon | Yes | **Fails closed** while the program stays pinned | Kernel must run the bpf LSM (Arch, Fedora, Debian 13 here; not Ubuntu by default); no prior art |
| 5 | **Guard: `chattr +i` on placeholders** | Stops writing back, not reading zeros | Blocks writing, deleting and renaming onto the file, so zeros can't be saved back | Daemon needs `CAP_LINUX_IMMUTABLE` and clears the flag before hydrating |
| 6 | **Monitor holds the fanotify group** across daemon restarts | – | Marks survive a crash, so no re-marking and no gap | Events being handled at the moment of the crash hang, killably, until restartable events land upstream [41] |
| 7 | **Placeholder under another name** (suffix file, broken symlink as in git-annex) | Yes | Fails closed by construction | Not transparent; defeats tether's purpose |

What does not work:

- **`MS_UNBINDABLE` / private propagation:** tested; full-tree copies still include the view.
- **Watching for new mounts (`FAN_MNT_ATTACH`):** the notification arrives after the copy is already readable.
- **fs-verity:** it would certify the zeros as valid content.
- **fscrypt:** locks the whole tree, not just placeholders.
- **Mandatory locks:** removed from the kernel.
- **Leases:** broken on a timeout and gone with the daemon.

## 6. fanotify vs. FUSE, fundamentally

The difference is **where tether sits relative to the real filesystem.**

- **FUSE:** tether *is* the filesystem. Every lookup, `stat`, readdir and open (and, without passthrough, every read and
  write) is a request to the daemon. The files on disk are a private backing store.
- **fanotify:** ext4, xfs or btrfs serves everything. The kernel asks tether only at the first access to a file that
  isn't downloaded yet. After that tether is out of the path, including for directory listings and `stat`.

| | fanotify (tether) | FUSE |
|---|---|---|
| Speed once local | Native for everything | Read/write near-native with passthrough; metadata goes through the daemon (Nydus measured `tar` metadata at 0.57 s in-kernel vs. 3.2 s over FUSE [27]) |
| Daemon dies | Local files keep working; placeholders need a guard | Whole folder `ENOTCONN`, including open files, until restart or fd handoff |
| Access-path coverage | Must be arranged (mount marks leaked; inode marks fix it) | Automatic |
| Data at rest | The folder *is* plain files: backups and other tools see real files | Backing store; the tree exists only while mounted |
| What it can present | Only what is on disk: every placeholder is a real inode; lazy readdir must wait for the kernel | Anything: lazy listings, virtual entries |
| Privileges | `CAP_SYS_ADMIN` | Unprivileged via `fusermount3`, but passthrough needs `CAP_SYS_ADMIN` |

FUSE gets correctness from its structure, at the cost of a userspace process permanently in the middle, for both speed
and availability. fanotify keeps userspace out of the steady state and keeps the data as plain files. In exchange,
tether has to earn correctness at the first-access boundary, and the per-placeholder memory above is part of that price.

## 7. Recommendation

1. **Switch to per-placeholder inode marks** (option 1).
   - The daemon does its own I/O through a detached, ignore-marked mount.
   - The separate view is no longer needed: users work in the folder itself, as in plain Syncthing.
   - Create each placeholder under a temporary name, mark it, then rename it into place.
2. **Let the monitor process hold the fanotify group** (option 6), so a crash-restart keeps the marks. Adopt
   `FAN_CONTROL_FD` when it lands.
3. **Always set `chattr +i` on placeholders** (option 5), so zeros can never be saved back. **Add the BPF LSM guard**
   (option 4) where the bpf LSM is active, so boot and a stopped service fail closed.
4. **Reduce the mark count:** download small files eagerly. A placeholder on ext4 already takes about 4 KB, so it saves
   nothing for small files. Measure memory on a real large tree before promising million-file trees.

FUSE (option 3) stays the fallback if lazy directory listings become a requirement before the kernel supports them.

## 8. Implementation notes (option 1)

What the implementation found beyond the spikes:

- **The reported path is the accessor's.** For a process in another mount namespace, the event fd's path is the path
  in *that* namespace (`/tmp/cx/p-docker`). So the listener identifies the folder and file by inode and uses paths
  only as hints. The candidates, in order:
  1. the path under a view or data directory;
  2. the name the placeholder was created under;
  3. every trailing part of the path.

  Each candidate must lead to the same inode. If none does, the access fails with `EIO`. That happens for a moved
  placeholder reached through a bind of its directory.
- **Eviction deadlocked for 45 s.** Eviction holds a write lease, and truncating a marked file raises a pre-access
  event. Delivering an event opens the file, which waits for our own lease to break. `MakePlaceholder` now marks for
  opens first and adds pre-access after the truncation. That is safe because the lease guarantees there are no other
  open files.
- **One read-only mount stopped the listener.** Read-write event fds can't be opened for accesses through a read-only
  mount (`docker -v …:ro`). The kernel denied the access, `read()` on the group failed with `EROFS`, and the listener
  exited, which left every later access blocked forever.
  - Fixes:
    - event fds are read-only;
    - `FAN_REPORT_FD_ERROR` reports per-event failures in the event itself;
    - the listener never stops while marks exist.
  - Content is written through a private detached clone of the data directory, carrying a mount ignore mark, after
    checking it is the event's inode.
- **The sync engine's own accesses** to marked placeholders are let through by process ID. That keeps the earlier
  behaviour, where the engine used an unmarked path.
- **Startup order:** probe for support (an `O_TMPFILE` inode), create the private clone, register the folder, mark
  every placeholder, then mount the view. On the V8 client, 19,442 placeholders were marked within the same second as
  startup.
- **Tests:**
  - listener tests for:
    - the data path;
    - another namespace's copy;
    - a moved placeholder through a container-style bind;
    - an unmatched path (fails with `EIO`);
    - existing placeholders at startup;
    - a read-only mount;
    - eviction under a lease finishing promptly;
  - the e2e test `other_mount_namespace_hydrates`, which fails on the previous release.

## 9. Implementation notes (option 6)

Prior art: the group lives while any process holds it, like `/dev/fuse` in FUSE's fd-store recovery pattern and in
Nydus's failover. Upstream's `FAN_CONTROL_FD` (restartable permission events) would re-queue in-flight events, but it is
unmerged as of September 2026.

- **Handover:** the monitor creates the group and passes it to each sync process as fd 3 (`TETHER_HSM_GROUP_FD`). A
  crashed sync process leaves the marks and the queue intact. The view stays mounted, and accesses wait until the next
  process reads them.
- **Stale events:** events the dead process had read are matched only by event fd number, so a new event could share
  a stale one's number. Before its first read, a restarted process therefore answers every possible number (up to
  `RLIMIT_NOFILE`, 524,288 here) with `EIO`. That takes about 40 ms. Nydus instead journals in-flight requests to retry
  them, which is a possible refinement.
- **Closing without taking events:** closing an fd does not wake a thread blocked in `read()` on it, so a closing
  listener could take one more event and strand it. The listener now polls the group together with an eventfd and
  reads without blocking under a lock that `Close` takes.
- **Measured on V8:**
  - `kill -9` during 16 parallel readers whose working directory was inside the view: 16 downloads in flight failed
    with `EIO`, 19,800 reads succeeded, none read zeros. Before: 11,200 zero-filled reads.
  - e2e `crash_keeps_placeholders_guarded`: 20 of 60 files read as zeros on the previous commit, 0 now.

## Sources

Most claims come from three survey agents working from primary sources where they could. Items verified directly for
this document:

- the erofs commit in torvalds/linux;
- the LSM lists on both test machines;
- everything in sections 1 and 4.

Some links are LKML mirrors (ratatoskr.run) because lore.kernel.org was blocked; [26]'s follow-up preprint was not
used.

1. https://learn.microsoft.com/en-us/windows/win32/cfapi/build-a-cloud-file-sync-engine
2. https://learn.microsoft.com/en-us/windows-hardware/drivers/ifs/placeholders_guidance
3. https://learn.microsoft.com/en-us/windows/win32/fileio/file-attribute-constants
4. https://learn.microsoft.com/en-us/answers/questions/4166541/error-0x8007016a-the-cloud-file-provider-is-not-ru
5. https://developer.apple.com/documentation/technotes/tn3150-getting-ready-for-data-less-files
6. https://keith.github.io/xcode-man-pages/setiopolicy_np.3.html
7. https://www.arqbackup.com/documentation/arq7/English.lproj/datalessFiles.html
8. https://dropbox.tech/infrastructure/going-deeper-with-project-infinite
9. https://www.macrumors.com/2022/01/27/macos-12-3-deprecates-dropbox-onedrive-kexts/
10. https://help.seafile.com/drive_client/drive_client_for_linux/
11. https://rclone.org/commands/rclone_mount/
12. https://www.pcloud.com/how-to-install-pcloud-drive-linux.html
13. https://github.com/jstaf/onedriver
14. https://help.nextcloud.com/t/how-do-virtual-files-work-in-the-client/154234
15. https://help.resilio.com/hc/en-us/articles/206115384-What-Is-an-RSLS-File
16. https://github.com/nextcloud/desktop/pull/10635
17. https://github.com/opencloud-eu/openvfs/blob/main/INTEGRATION.md
18. https://github.com/facebook/sapling/blob/main/eden/fs/docs/Overview.md
19. https://github.com/facebook/sapling/blob/main/eden/fs/docs/Takeover.md
20. https://github.com/facebook/sapling/blob/main/eden/fs/docs/Windows.md
21. https://github.com/github/libprojfs/blob/master/docs/design.md
22. https://github.com/microsoft/VFSForGit/issues/126
23. https://github.blog/open-source/git/the-story-of-scalar/
24. https://cvmfs.readthedocs.io/en/2.11/cpt-details.html
25. https://kubernetes.web.cern.ch/blog/2023/12/18/autofs-in-containers/
26. https://github.com/containerd/stargz-snapshotter/issues/2387
27. https://d7y.io/blog/2022/06/06/evolution-of-nydus/
28. https://lwn.net/Articles/911177/
29. https://github.com/LiXi-storage/lustre_manual_markdown/blob/master/26-Hierarchical%20Storage%20Management%20(HSM).md
30. https://help.dropbox.com/installs-integrations/sync-uploads/smart-sync
31. https://support.google.com/drive/answer/2375082
32. https://ratatoskr.run/lkml/2026/06/17161901/t
33. https://ratatoskr.run/linux-cve-announce/2026/09/17515914
34. https://lwn.net/Articles/983376/
35. https://lwn.net/Articles/981392/
36. https://github.com/amir73il/fsnotify-utils/wiki/Hierarchical-Storage-Management-API
37. https://lore.kernel.org/linux-fsdevel/20210510101305.GC11100@quack2.suse.cz/t/
38. https://github.com/josefbacik/remote-fetch
39. https://github.com/torvalds/linux/blob/master/fs/notify/fsnotify.c
40. https://www.man7.org/linux/man-pages/man7/fanotify.7.html
41. https://lwn.net/Articles/1075829/
42. https://ratatoskr.run/linux-fsdevel/2026/04/347570/t
43. https://docs.kernel.org/filesystems/fuse/fuse-passthrough.html
44. https://ratatoskr.run/linux-unionfs/2026/05/12525594/t
45. https://source.android.com/docs/core/storage/fuse-passthrough
46. https://docs.kernel.org/bpf/prog_lsm.html
47. https://raw.githubusercontent.com/torvalds/linux/master/fs/bpf_fs_kfuncs.c
48. https://gitlab.archlinux.org/archlinux/packaging/packages/linux/-/raw/main/config.x86_64
49. https://src.fedoraproject.org/rpms/kernel/raw/rawhide/f/kernel-x86_64-fedora.config
50. https://bugs.launchpad.net/bugs/2036281
