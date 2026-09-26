# Keeping placeholders guarded while the daemon restarts: survey and design

Every placeholder's inode carries a fanotify mark; see [placeholder-access-paths.md](placeholder-access-paths.md).
Marks belong to a fanotify group, and when the last reference to the group closes, the kernel drops every mark and
allows every pending event. Before this work, the sync process owned the group, so each crash or restart of that
process exposed placeholders as zeros until the next process had marked them again.

## 1. What we found (September 2026)

Setup: the V8 tree (19,816 files) on the Arch client. 16 parallel readers hashed every online-only file with their
working directory inside the view, and the sync process was killed with `kill -9` after 4 s.

| | Before (sync process owns the group) | After (monitor owns it, commit 64be156) |
|---|---|---|
| Reads with zero-filled content | **11,200** (all 200 sampled mismatches were all zeros) | **0** |
| Reads that failed | 0 | 16 (`EIO`: the downloads in flight at the crash) |
| Reads that matched the hub | 8,616 | 19,800 |

Zeros were read despite the monitor unmounting the view. The unmount is lazy, so processes whose working directory is
inside the view (shells, editors, builds) keep using the detached mount. The e2e test
`crash_keeps_placeholders_guarded` reproduces this: 20 of 60 files read as zeros on the previous commit, 0 now.

## 2. Kernel semantics

Line references are to `fs/notify/fanotify/fanotify_user.c` ("FU") and `fanotify.c` ("F") in torvalds/linux master,
as of 2026-09-25 [1–2].

- **A group lives until the last reference to its file closes.** `fanotify_release`, the file's `.release`, runs only
  on the final `fput` (FU 1089–1135, 1180). If another process holds a duplicate (inherited, `SCM_RIGHTS`, or
  `pidfd_getfd`), a `kill -9` of the reader closes only its own descriptor. Marks, queued events and read-but-unanswered
  events all survive.
- **When the last reference closes, every pending event is allowed** (FU 1107–1132). This is why fanotify daemons
  such as fapolicyd and ClamAV's on-access scanner fail open when they die, and why tether did.
- **Read-but-unanswered events stay pending, and any reader can answer them.** A permission event moves to the
  group's `access_list` when it is read (FU 1039–1041). A response is matched only by the event fd number:
  `if (event->fd != fd) continue;` (FU 494–506). Nothing ties it to the process that read the event. Unknown numbers
  return `ENOENT` (FU 516).
- **Collision hazard:** a new reader's event can get the same fd number as a stale event. The list is searched from
  its head, so an answer meant for the new event releases the stale one instead, and the new event waits forever.
- **Queued events that nobody has read** are delivered to the next `read()` on any descriptor of the group (FU 887–947).
- **Waiting accessors are killable, and nothing else interrupts them:** `TASK_KILLABLE|TASK_FREEZABLE` (F 230–256).
  The permission-event watchdog only logs, and is off by default (`watchdog_timeout` = 0) (FU 54, 150–165). Red Hat
  documents whole-system hangs from unanswered permission events [3].
- **Pre-content groups can deny with a chosen errno** via `FAN_DENY_ERRNO`: EIO, EPERM, EBUSY, ETXTBSY, EAGAIN, ENOSPC
  or EDQUOT (FU 454–475) [4].
- **Closing a descriptor does not wake a thread blocked in `read()` on it.** We found this during implementation, not
  in the survey. A listener being closed could take one more event and strand it.

## 3. Upstream: restartable permission events

Ibrahim Jirdeh (Meta) posted the series in August 2025. v2 (`FAN_RESTARTABLE_EVENTS`) [5] became v3
(`FAN_CONTROL_FD`, April 2026) [6] at Jan Kara's and Amir Goldstein's request:

- **Control fd:** the fd from `fanotify_init` keeps the group alive and accepts `fanotify_mark`.
- **Queue fd:** `FAN_IOC_OPEN_QUEUE_FD` opens the fd you read events from and write responses to. Only one can exist
  at a time.
- **Re-queueing:** when the queue fd is released, read-but-unanswered events go back to the head of the queue.
- **Intended use:** a separate process such as systemd's fd store keeps the control fd; a restarted daemon opens a
  fresh queue fd and gets the pending events again [7].

**Status: not merged as of 2026-09-25.** It isn't in the uapi header, the 7.2-rc1 and 7.3-rc1 fsnotify pulls don't
include it, and the last v3 activity was July 2026 [1, 8–9]. The earliest possible release is 7.4 (unverified). No
event-ID response mechanism exists either; the only non-fd response form is `FAN_NOFD` with `FAN_INFO` (FU 487–490).

## 4. Prior art

| System | What survives a restart | In-flight requests | Source |
|---|---|---|---|
| **systemd fd store** | fds sent with `FDSTORE=1`, returned via `LISTEN_FDS`. Kept across `systemctl restart`, not across stop+start unless `FileDescriptorStorePreserve=yes`, never across reboot | – | [10] |
| **FUSE** | An fd-store holder keeps `/dev/fuse` open; requests wait in the queue meanwhile | `FUSE_NOTIFY_RESEND` (Linux 6.9) re-sends unanswered requests | [11–12] |
| **Nydus failover** | An external holder keeps `/dev/fuse` plus a shared in-flight request journal (`SCM_RIGHTS`) | Without resend, the successor answers the journaled request IDs with EIO | [13] |
| **cachefiles on-demand** | fds kept via Unix sockets; the new daemon writes `restore` | Re-issued (mode being removed in 2026) | [14–15] |
| **EdenFS takeover** | Mount and inode state plus fds, passed on graceful restart | Crash case not documented | [16] |
| **CernVM-FS hotpatch** | Reloads a library while the loader queues filesystem calls | Queued | [17] |
| **stargz `fuse_manager`** | Holds the FUSE fds, but only with `KillMode=process`; otherwise `systemctl restart` kills it | – | [18] |
| **fapolicyd, ClamAV on-access** | Nothing: the group closes and everything is allowed. ClamAV forbids prevention mode on `/` because it locks the system up | – | [19–20] |

## 5. Guards for when no daemon is running

- **BPF LSM.** A `file_open` program pinned in bpffs stays attached after its loader exits, but not across reboot
  [21–22].
  - **Requirements:** `bpf` in the active LSM list. It is on both tether test machines (Arch 7.2, Debian 13).
  - **Reading the placeholder tag:** `bpf_get_file_xattr` (6.8+) reads `user.*` and `security.bpf.*` from sleepable
    LSM programs. `security.bpf.*` can be set only by BPF, so users can't forge or strip it [23–24].
  - **Production examples:** systemd `RestrictFileSystems=` [25] and Tetragon's persistent enforcement [26].
  - **Ordering:** `security_file_open` runs before `fsnotify_open_perm`. A guard must therefore deny only when no
    group has the inode marked, for example by reading `i_fsnotify_mask` through BTF (unverified), or tether's own
    accesses would be starved.
- **Leases are not a fit** [27]:
  - a read lease never blocks readers;
  - a write lease needs no other open fds and is force-broken after `lease-break-time` (45 s), after which readers see
    zeros;
  - it costs one fd per placeholder and dies with its holder.
- **Mandatory locks** were removed in Linux 5.15.

## 6. Options and what tether does

What each option does in four cases: an access during the restart gap, an event in flight at the crash, a full stop of
the service, and a reboot.

| Option | Gap | In flight at crash | Full stop / monitor crash | Reboot |
|---|---|---|---|---|
| **A. Monitor owns the group, stale events cleared with EIO** | Waits (killable) for the next process | EIO | Group closes: zeros | Zeros until marked |
| B. A + systemd fd store | Waits, including across a monitor crash and `systemctl restart` | EIO | With `Preserve=yes`, readers wait indefinitely; otherwise zeros | Zeros until marked |
| C. A or B + pinned BPF LSM guard | As A/B | As A/B | EIO | EIO once an early unit loads the guard |
| D. Upstream `FAN_CONTROL_FD` | Waits | Re-queued and retried | As B | As B |
| E. Leases | Not a fit | – | – | – |

**tether implements A** (commit 64be156):

- **Handover:** the monitor (`syncthing serve`) creates the group and passes it to each sync process as fd 3
  (`TETHER_HSM_GROUP_FD`). The view stays mounted across a crash, and the monitor unmounts views only when it exits
  itself.
- **Stale events:** before its first read, a restarted process answers every possible event fd number, up to
  `RLIMIT_NOFILE`, with `FAN_DENY_ERRNO(EIO)`. For 524,288 numbers this takes 35–47 ms, so a journal of in-flight
  event numbers (the Nydus approach) wasn't needed to bound the sweep. A journal would let the successor retry
  interrupted downloads instead of failing them.
- **Closing:** the listener polls the group together with an eventfd, and reads without blocking under a lock that
  `Close` takes. A closing listener therefore never takes an event, and never answers after close.
- **Tests:** `TestGroupSurvivesListener`, e2e `crash_keeps_placeholders_guarded`, and the V8 run in §1.

**tether also implements C** (the placeholder guard, `lib/hsm/bpf/guard.bpf.c`):

- **What it checks:** a sleepable `lsm.s/file_open` program. An open fails with `EIO` when no live group has the inode
  marked for `FS_OPEN_PERM` **and** the file carries `user.tether.state=virtual`.
  - **"Live" matters.** When a group closes, the kernel flags it `shutdown` and then detaches its marks one by one.
    Until an inode's mark is gone, its `i_fsnotify_mask` still says "marked", but the dying group raises no events.
  - **Measured on the V8 client** (16,678 placeholders, `kill -9` of the service, one reader looping over 51
    placeholders): the first guard trusted the mask, and 161 opens read zeros for up to 11 ms.
  - **The fix:** the guard now walks the inode's marks with probe reads and accepts only a mark whose group is not
    shutting down. The same measurement gives 1 zero read, three times out of three: the open that was already under
    way.
  - **What remains** (`TestGuardDuringTeardown`, 20,000 placeholders, 8 reader threads): at most one open per thread
    that passed the guard just before the shutdown. The guard runs in `security_file_open`, a few instructions before
    `fsnotify_open_perm`, which then finds the group gone. The old guard let about 7,500 through.
  - **Reads already waiting** when the group dies are released by the kernel (`fanotify_release` answers them with
    `FAN_ALLOW`) and read whatever the placeholder holds.

  Both remaining cases need the monitor itself to die, since a crash of the sync process keeps the group. Both need
  the kernel's help to close; see D.
- **Cost for other files:** marked inodes and files with data blocks return before the xattr lookup. 200,000 opens
  of a small file changed by ±50 ns per open, which is within noise.
  - **The exception:** a file being hydrated carries `chattr +A` (`S_NOATIME`) until the hydration finishes or is
    discarded. The guard reads the xattr of such files even though they have data blocks, so a placeholder that a
    crash left partly written is still refused ([stale-placeholders.md](stale-placeholders.md) §5).
- **Buffer:** the xattr value goes into task-local storage. Dynptrs can't point at the stack, and a per-CPU buffer
  could be overwritten while the sleepable program sleeps.
- **Installation:**
  - The monitor installs it at every start, with only `CAP_SYS_ADMIN`.
  - It pins it in a bpffs instance that it mounts at `<data dir>/guard`, since `/sys/fs/bpf` is root-only. A mount
    made by a user service lives in the host's mount namespace, so it survives the service.
  - Reinstalling pins the new link under a temporary name and renames it over the old pin, so there is no gap. bpffs
    rejects names containing dots.
- **Timing:** it takes effect about 1 ms after the group closes (marks are torn down asynchronously), and detaches
  about 10 ms after its pin is removed.
- **Results:**
  - `TestGuard` passes on ext4, btrfs and xfs.
  - On the V8 client, with `systemctl --user stop`, a shell whose working directory was inside the view got `EIO`
    instead of zeros, as did reads through the data directory. After a start, the same files downloaded and matched
    the hub.

**Also implemented: the group outlives the monitor under systemd** (the service manager's
[file descriptor store](https://systemd.io/FILE_DESCRIPTOR_STORE/), the documented way to keep an fd across a
service's restarts):
- The monitor hands a copy of the group to systemd (`FDSTORE=1`, `FDNAME=tether-hsm-group`; the unit needs
  `FileDescriptorStoreMax=1` and `NotifyAccess=main`). After a crash, systemd passes the group back (`LISTEN_FDS`,
  `LISTEN_FDNAMES`) to the restarted monitor.
- So a `kill -9` of the whole service no longer closes the group: waiting accesses stay blocked, and nothing is
  allowed through by the kernel. The marks stay too, so the restart does not re-mark anything.
- The new sync process fails the accesses that were interrupted with `EIO` (the stale-event sweep). New accesses are
  served.
- **Measured on the V8 client:** a reader in the middle of hydrating the 36 MB pack got partial data (exit status 0)
  before this change. Now it gets `EIO` about 0.5 s after the kill, and the next read gets the right content.
- **An explicit stop** still closes the group; systemd flushes the store once the unit is inactive. The monitor now
  fails the accesses still waiting first (`hsm.DenyPending`: queued events, plus a sweep for events the exited sync
  process had read). Otherwise `fanotify_release` would allow them and they would read whatever the placeholder
  held. Live: a reader mid-download at `systemctl --user stop` got `EIO`, and its partial content was discarded.
  Test: `TestDenyPending`.
- **Without systemd, or a kill followed by a stop,** the group dies with the monitor as before, and the guard's
  limits apply.

**Still open:**

- **Boot:** until tether starts after a reboot, nothing guards placeholders. An early root unit that loads the guard
  would close that window.
- **D:** adopt upstream `FAN_CONTROL_FD` when it lands; it would replace the EIO sweep with retries.
- **`chattr +i`** is not a viable interim step. An immutable file can't be renamed, deleted or chmod'ed, and tether
  supports those operations on placeholders.

## Sources

Some links are LKML mirrors (ratatoskr.run), because lore.kernel.org blocked the survey agent. The survey could not
verify:

- the v1 thread of the restartable-events series;
- fapolicyd's deadman-switch trigger;
- how Sophos or CrowdStrike handle restarts;
- which version added `security.bpf.*`;
- whether BPF can reliably read `i_fsnotify_mask`.

1. https://github.com/torvalds/linux/blob/master/fs/notify/fanotify/fanotify_user.c
2. https://github.com/torvalds/linux/blob/master/fs/notify/fanotify/fanotify.c
3. https://access.redhat.com/solutions/5201171
4. https://github.com/torvalds/linux/blob/master/include/uapi/linux/fanotify.h
5. https://ratatoskr.run/linux-fsdevel/2026/04/347303/t
6. https://ratatoskr.run/linux-fsdevel/2026/04/347570/t
7. https://lwn.net/Articles/1075829/
8. https://ratatoskr.run/linux-fsdevel/2026/06/17134040/t
9. https://ratatoskr.run/linux-fsdevel/2026/08/17434438/t
10. https://systemd.io/FILE_DESCRIPTOR_STORE/
11. https://lwn.net/Articles/974925/
12. https://app.opencve.io/cve/CVE-2024-38626
13. https://github.com/dragonflyoss/nydus/pull/2058
14. https://lwn.net/Articles/911177/
15. https://ratatoskr.run/netfs/2026/08/17402592/t
16. https://github.com/facebook/sapling/blob/main/eden/fs/docs/Takeover.md
17. https://cvmfs.readthedocs.io/en/stable/cpt-configure.html
18. https://github.com/containerd/stargz-snapshotter/issues/2387
19. https://github.com/linux-application-whitelisting/fapolicyd/issues/237
20. https://docs.clamav.net/manual/OnAccess.html
21. https://docs.ebpf.io/linux/concepts/pinning/
22. https://ebpf-go.dev/concepts/object-lifecycle/
23. https://github.com/torvalds/linux/blob/master/fs/bpf_fs_kfuncs.c
24. https://github.com/torvalds/linux/commit/ac9c05e0e453cfcab2866f6d28f257590e4f66e5
25. https://github.com/systemd/systemd/pull/18145
26. https://tetragon.io/docs/concepts/enforcement/persistent-enforcement/
27. https://man7.org/linux/man-pages/man2/F_SETLEASE.2const.html
