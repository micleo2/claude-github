# tether

Dropbox-style selective sync for Linux, built as a fork of [Syncthing](https://syncthing.net).

Every client sees the whole tree, but a file's content is downloaded only when something opens it, or when you pin it.
Every copy of your data is **ordinary files on an ordinary filesystem**. That includes the server, the clients, and
"online-only" files, which are sparse placeholders of the right size and mtime. There is no block store and no custom
container format.

This works through **fanotify pre-content events** (Linux ≥ 6.14). When a program opens a placeholder, the kernel
blocks it while tether fetches the content from a peer, verifies every block and writes it in place. After that the
program reads a normal local file at native speed.

- **Setting up a server and clients:** [docs/setup.md](docs/setup.md)
- **Design and rationale:** [PLAN.md](PLAN.md)
- **Kernel-level findings (Phase 0):** [spikes/fanotify-hsm/FINDINGS.md](spikes/fanotify-hsm/FINDINGS.md)
- **Small-file performance survey:** [docs/research/small-file-hydration.md](docs/research/small-file-hydration.md)
- **Placeholders seen from other mount namespaces (survey, spikes, options):**
  [docs/research/placeholder-access-paths.md](docs/research/placeholder-access-paths.md)
- **Eviction (survey and measurements):** [docs/research/eviction.md](docs/research/eviction.md)
- **Keeping placeholders guarded across daemon restarts:** [docs/research/daemon-restart.md](docs/research/daemon-restart.md)
- **Stale placeholders and failed hydrations (survey, churn stress test):** [docs/research/stale-placeholders.md](docs/research/stale-placeholders.md)
- **Next step, crawler-aware prefetch:** [docs/design/crawler-prefetch.md](docs/design/crawler-prefetch.md)

Build with `cd tether && go run build.go build` (plus `go build ./cmd/tether` for the CLI).

## Layout

| Path | What |
|---|---|
| `tether/` | The Syncthing fork (from v2.1.5; see `tether/UPSTREAM`) |
| `tether/lib/hsm/` | fanotify listener with per-placeholder inode marks, bind-mount views, placeholder xattrs, lease-based eviction helpers |
| `tether/lib/model/folder_ondemand.go` | Placeholder creation, hydration from peers, eviction, pins, cache budget |
| `tether/lib/model/model_ondemand.go` | Listener lifecycle, hydration policy, `/rest/ondemand/*` backing |
| `tether/cmd/tether/` | `tether` CLI (`status`, `pin`, `unpin`, `evict`, `hydrate` by path) |
| `e2e/` | Multi-node Docker end-to-end suite |
| `spikes/fanotify-hsm/` | Phase 0 kernel spike and its conformance matrix |

## Using it

Step-by-step setup is in [docs/setup.md](docs/setup.md). In short:

Requirements for on-demand folders: Linux ≥ 6.14, and `CAP_SYS_ADMIN` for the daemon (fanotify pre-content groups
are privileged). The folder must also be on a filesystem that supports pre-content events:
- **What decides it:** tether has no filesystem-specific code. The kernel only allows these events on filesystems that
  opt in: ext4 (which also serves ext2/ext3), xfs and btrfs.
- **Tested:** ext4, xfs and btrfs pass the kernel-level checks under QEMU on 6.14 and 6.17 (`e2e/vm/run.sh`).
  tmpfs, ramfs and overlayfs refuse with `EOPNOTSUPP`.

If any requirement is missing, the folder refuses to start and reports an error. It never degrades to placeholders that nothing can fill. The server needs none of this: it holds
plain files.

A client folder is a normal send-receive folder with four extra settings:

```xml
<folder id="docs" path="/var/lib/tether/docs" type="sendreceive">
    <onDemand>true</onDemand>
    <onDemandView>/home/me/Docs</onDemandView>   <!-- where you work -->
    <pinPattern>/Projects/current</pinPattern>    <!-- always local; ignore-file syntax -->
    <cacheBudget>20 GB</cacheBudget>              <!-- or "10 %"; 0 = unlimited -->
</folder>
```

- **`path`** is used by the sync engine.
- **`onDemandView`** is a bind mount of it that tether creates, and where you work. Every placeholder's inode carries
  a fanotify mark, so opening it through any path downloads it first: the view, `path`, a container's bind mount or a
  sandbox (read-only mounts included).

```sh
tether status ~/Docs/Photos      # local / pinned / online-only per file
tether pin ~/Docs/Photos/2026    # download now and keep local
tether unpin ~/Docs/Photos/2026
tether evict ~/Docs/Videos       # free space; files stay listed and openable (-verify re-reads them first)
tether hydrate ~/Docs/Report.pdf # download now without pinning
```

The same operations are available over REST under `/rest/ondemand/{status,pin,unpin,evict,hydrate}?folder=&path=`.

## How it behaves

- **Opening** a placeholder hydrates the whole file before `open()` returns. That includes `read`, `mmap`, `exec`,
  `cp`, `rsync` and editors.
  - Tools that probe holes (`cp`, `rsync -S`, `tar -S`) would otherwise copy zeros. Hydrating at open time makes them
    correct.
  - Listing, `stat`, `ls -l` and `du --apparent-size` never download anything.
- **Renaming, moving, `chmod` or deleting** a placeholder propagates to the cluster without downloading it.
  Placeholders record which content they stand for, so the scanner never reads them.
- **Editing** a placeholder hydrates it first, so local changes always start from the real content.
- **Remote changes:**
  - Pinned files are downloaded eagerly.
  - Other files become placeholders of the new version, even if they were local before.
- **Eviction** turns a file back into a placeholder, in batches of 1000 index updates (V8's 19,816 files take about
  2 s). It is refused when:
  - the file is open (checked with a write lease), or
  - its size or modification time differs from the index (with `-verify`, any block), or
  - no other device holds that exact version.
- **Sibling prefetch:** when an application opens a placeholder, the small placeholders in the same directory are
  downloaded in the background by a pool of workers.
  - **Limits:** direct children only, files ≤ `prefetchMaxFileKiB` (default 256; 0 disables), `prefetchConcurrency`
    workers (default 16), each directory at most once per 30 s.
  - **Shared downloads:** an application opening a file that is being prefetched waits for that download rather than
    starting another.
  - **Effect:** `grep -r` over 300 online-only files with 20 ms round-trip time went from 9.2 s to 2.3 s.
- **Cache budget:** when local content exceeds the budget, the least recently used unpinned files are evicted down
  to 80% of the budget, so a folder near its budget isn't cleaned in many small rounds. Prefetched files that nobody
  opened are evicted first.
- **Failure handling:**
  - **Offline:** opening a placeholder fails with an error and never returns zeros. Local files keep working.
  - **Peer silently gone:** reads fail after `hydrationTimeoutS` (default 60 s).
  - **Reconnect in progress:** reads wait for a peer that is still connecting.
- **Indexers** (tracker, baloo, updatedb/plocate, …) get `EPERM` instead of downloading everything. Extend the list
  with `<hydrationDenyExe>`.
- **Containers and sandboxes** (docker `-v`, flatpak/bwrap, `unshare -m`) download on open like any other process,
  because the marks are on the placeholders' inodes, not on a mount. A placeholder that was moved and is then reached
  through a bind of its directory can't be identified from the path the kernel reports; it fails with `EIO`.
- **Crash safety:** the monitor process (`syncthing serve`) owns the fanotify group and hands it to every sync process
  it starts, so the marks survive a crash of the sync process. Accesses meanwhile wait for the restarted process
  (about 1–2 s). Downloads in flight at the crash fail with `EIO`. In a `kill -9` during 16 parallel readers of V8,
  16 reads failed and none read zeros (before: 11,200 zero-filled reads).

## Testing

```sh
sudo spikes/fanotify-hsm/run-tests.sh   # kernel conformance: 28 access paths
cd tether && go test ./lib/hsm/          # listener unit tests (root)
sudo e2e/run.sh                          # 42 end-to-end tests, ~5 min
sudo e2e/run.sh --slow                   # bigger trees / files
```

The e2e suite runs a server and two on-demand clients in privileged Docker containers. Each node is backed by its own
loop-mounted ext4 filesystem. The suite covers:

- hydration through every access path
- concurrent readers
- remote updates, pins and local edits, and placeholders superseded before the puller catches up
- rename, move, `chmod` and delete of placeholders
- directory renames and atomic-save editors
- eviction, including the refusals
- offline and silent network loss
- peers never serving placeholders
- indexer denial and the cache budget
- `kill -9` recovery, interrupted hydrations and client restart
- conflicts
- the CLI
- a final check that no file ever became zeros

Syncthing's own test suites pass, except `TestHostCheck` (needs IPv6 loopback) and `TestIsLANHost` (depends on the
host's network). Both fail identically on unmodified upstream in this environment.

## Known limitations

- **While tether is stopped, placeholders fail with `EIO`, if the kernel has the bpf LSM.** Stopping the service, a
  crash of the monitor process, or a reboot closes the fanotify group, and the marks go with it (they are re-added at
  startup, 1.5–2.2 s per million).
  - **The guard:** the monitor installs a BPF LSM program that fails opens of unmarked placeholders with `EIO`, from
    any path. It is pinned in a bpffs mount at `<data dir>/guard`, so it outlives the service. It needs `bpf` in
    `/sys/kernel/security/lsm` (Arch, Fedora and Debian 13 have it; Ubuntu doesn't by default) and Linux 6.8 or later.
  - **Without the bpf LSM,** placeholders read as zeros while tether is stopped: through `path`, through copies of the
    view held by containers or sandboxes, and from any process whose working directory was inside the view.
  - **After a reboot,** the guard is gone until tether starts again.
  - **Reads already waiting** for a download when the group closes are released by the kernel ("allow") and may return
    zeros or partial data. So may an open that races the shutdown by microseconds.
    - **A clean stop avoids this:** the monitor fails them with `EIO` first.
    - **So does a crash of the sync process:** the monitor keeps the group.
    - **So does a crash of the monitor under systemd,** with `FileDescriptorStoreMax=1` and `NotifyAccess=main` (see
      [docs/setup.md](docs/setup.md)): systemd keeps a copy of the group until the service restarts.
    - **What remains:** the monitor dying without systemd, or a kill followed by a stop.

  See [docs/research/daemon-restart.md](docs/research/daemon-restart.md).
- **Kernel memory:** a marked inode can't be evicted, so each online-only file holds about 1.3 KB of kernel memory.

- **Directory listings are materialised.** Every client creates a placeholder for every file. That costs inodes, and
  metadata, but no data blocks. Measured cost per placeholder:
  - ext4: about 4.1 KB, because the xattrs don't fit in a 256-byte inode
  - xfs: about 550 B
  - btrfs: about 1.4 KB

  The kernel has no pre-content hook for `readdir`/`lookup` yet, so listings can't be populated lazily.
- **Whole-file hydration.** Range hydration works at the kernel level (see the spike), but the daemon downloads whole
  files. Opening a large file waits for all of it.
- **The first file opened in each directory still waits one full fetch.** Its siblings are prefetched, but tools that
  touch one file per directory get no benefit. Deeper prefetch (crawler detection, access history) is future work; see
  [docs/research/small-file-hydration.md](docs/research/small-file-hydration.md).
- **A metadata-only change to a placeholder can win a conflict against a content change.** This needs the content
  change to carry an *older* mtime. The winning version's content then exists nowhere. The losing content is kept as a
  conflict copy, and the file cannot be hydrated until someone writes it again.
- **A file that changes on its source faster than it can be fetched** may fail to open with `EIO`: the source no
  longer has the old version, and has not announced the new one yet. Superseded placeholders otherwise hydrate at the
  latest version. In a stress test with 20 rewrites a second, 0.2% of reads failed and none returned wrong data. See
  [docs/research/stale-placeholders.md](docs/research/stale-placeholders.md).
- **Ignore patterns (`.stignore`)** are not tested with on-demand folders and not supported there.
- **Privileges.** The daemon runs with `CAP_SYS_ADMIN`. The split into a small privileged helper and an unprivileged
  sync daemon (PLAN.md §4.6) is not implemented yet.
- **Only ext4 has been exercised end to end.** xfs and btrfs pass the kernel-level checks under QEMU, but the
  multi-node suite has only run on ext4.
