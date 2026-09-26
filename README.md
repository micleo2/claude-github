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
tether evict ~/Docs/Videos       # free space; files stay listed and openable
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
- **Eviction** turns a file back into a placeholder. It is refused when:
  - the file is open (checked with a write lease), or
  - its content differs from the index, or
  - no other device holds that exact version.
- **Sibling prefetch:** when an application opens a placeholder, the small placeholders in the same directory are
  downloaded in the background by a pool of workers.
  - **Limits:** direct children only, files ≤ `prefetchMaxFileKiB` (default 256; 0 disables), `prefetchConcurrency`
    workers (default 16), each directory at most once per 30 s.
  - **Shared downloads:** an application opening a file that is being prefetched waits for that download rather than
    starting another.
  - **Effect:** `grep -r` over 300 online-only files with 20 ms round-trip time went from 9.2 s to 2.3 s.
- **Cache budget:** when local content exceeds the budget, the least recently used unpinned files are evicted.
  Prefetched files that nobody opened are evicted first.
- **Failure handling:**
  - **Offline:** opening a placeholder fails with an error and never returns zeros. Local files keep working.
  - **Peer silently gone:** reads fail after `hydrationTimeoutS` (default 60 s).
  - **Reconnect in progress:** reads wait for a peer that is still connecting.
- **Indexers** (tracker, baloo, updatedb/plocate, …) get `EPERM` instead of downloading everything. Extend the list
  with `<hydrationDenyExe>`.
- **Containers and sandboxes** (docker `-v`, flatpak/bwrap, `unshare -m`) download on open like any other process,
  because the marks are on the placeholders' inodes, not on a mount. A placeholder that was moved and is then reached
  through a bind of its directory can't be identified from the path the kernel reports; it fails with `EIO`.
- **Crash safety:** the view exists only while the listener runs. If the daemon is killed, the monitor process unmounts
  it, so newly opened paths no longer lead to placeholders. The unmount is lazy, though: see the limitation below.

## Testing

```sh
sudo spikes/fanotify-hsm/run-tests.sh   # kernel conformance: 28 access paths
cd tether && go test ./lib/hsm/          # listener unit tests (root)
sudo e2e/run.sh                          # 39 end-to-end tests, ~4 min
sudo e2e/run.sh --slow                   # bigger trees / files
```

The e2e suite runs a server and two on-demand clients in privileged Docker containers. Each node is backed by its own
loop-mounted ext4 filesystem. The suite covers:

- hydration through every access path
- concurrent readers
- remote updates, pins and local edits
- rename, move, `chmod` and delete of placeholders
- directory renames and atomic-save editors
- eviction, including the refusals
- offline and silent network loss
- peers never serving placeholders
- indexer denial and the cache budget
- `kill -9` recovery and client restart
- conflicts
- the CLI
- a final check that no file ever became zeros

Syncthing's own test suites pass, except `TestHostCheck` (needs IPv6 loopback) and `TestIsLANHost` (depends on the
host's network). Both fail identically on unmodified upstream in this environment.

## Known limitations

- **While the daemon is down, placeholders read as zeros.** Marks live in the kernel only while the daemon runs, and it
  marks every placeholder again at startup (1.5–2.2 s per million). Until then placeholders read as zeros through
  `path`, through copies of the view held by containers or sandboxes, and from any process whose working directory is
  inside the view (the lazy unmount leaves it reachable). A `kill -9` during parallel reads of V8 produced 11,200
  zero-filled reads. Planned: the monitor
  keeps the fanotify group across crashes, `chattr +i` on placeholders, and a BPF LSM guard; see
  [docs/research/placeholder-access-paths.md](docs/research/placeholder-access-paths.md).
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
- **Ignore patterns (`.stignore`)** are not tested with on-demand folders and not supported there.
- **Privileges.** The daemon runs with `CAP_SYS_ADMIN`. The split into a small privileged helper and an unprivileged
  sync daemon (PLAN.md §4.6) is not implemented yet.
- **Only ext4 has been exercised end to end.** xfs and btrfs pass the kernel-level checks under QEMU, but the
  multi-node suite has only run on ext4.
