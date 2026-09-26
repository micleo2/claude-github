# Hydrating many small files: industry survey and what it means for tether

Problem: `grep -r`, a build, an IDE indexer, or opening a folder can touch thousands of online-only files. Each open
blocks until that file is fetched, and most tools open files one at a time, so latency adds up per file.

## 1. What we measured first (September 2026)

Setup: `grep -r` over 300 online-only files of about 300 B each, client and server in Docker on one host, ext4.

| | Per file | 300 files |
|---|---|---|
| Before | ~11.7 ms | 3.5 s |
| After batching index updates (commit 8dac6ef) | ~2.6 ms | 0.78 s |

The per-file breakdown before the change explained most of the cost:

| Step | Median |
|---|---|
| Index lookup | 0.2 ms |
| Network fetch from the hub (block request + verify) | 0.7 ms |
| `fsync` | 1.1 ms |
| Clear xattrs, restore mtime | 0.06 ms |
| Index update (one SQLite transaction) | 1.4 ms |
| **Blocked on Syncthing's event bus** (`eventLogTimeout` = 15 ms for a slow subscriber) | **up to 15 ms** |

The fix unblocks the application once the content is durable and the placeholder attributes are removed. Index
updates are committed in 100 ms batches, and the scanner repairs the index if the process dies before a commit.

**What remains per file:** the synchronous fetch (network round trip plus request handling) and one `fsync`. On a LAN
that's about 2–3 ms. Over a 30 ms WAN the round trip would dominate, which is exactly what the techniques below are for.

## 2. What other systems do

The network proxy blocked some primary sources (learn.microsoft.com, dropbox.tech, lwn.net, lore.kernel.org,
rclone.org, git-scm.com). Microsoft and Apple docs were read from their GitHub or JSON mirrors. Items marked *(snippet)*
rest on a search-result excerpt only, and **unverified** marks claims with no source found.

| System | Mechanism against per-file latency | Notes |
|---|---|---|
| **Windows Cloud Files API** (OneDrive, Nextcloud, SeaDrive on Windows) [1–6] | See below | No published small-file threshold. Whether OneDrive batches or prefetches siblings is **unverified**. |
| **Apple File Provider** (Dropbox on macOS, iCloud Drive) [7–10] | See below | |
| **Dropbox** [11] *(snippet)* | Block-server `store_batch`/`retrieve_batch`; prefetching blocks of files not yet committed (up to 2× faster sync) | No public post on small-file prefetch for online-only files (**unverified**) |
| **VFS for Git / Scalar** [12–15] | Commits and trees fetched in bulk as "prefetch packs"; file contents on demand; `POST /gvfs/objects` batches many objects per request; `/gvfs/sizes` answers listings without content; `gvfs prefetch --folders X --hydrate` | Visual Studio IntelliSense crawling 10k files hung the OS (about 4000 blocked threads); `--files '*' --hydrate` hangs on big repos |
| **git partial clone** [16–17] *(snippet)* | Checkout bulk-prefetches all missing blobs in **one batch**, because on-demand fetching goes one object at a time; `git backfill` fetches in batches grouped by path | |
| **Meta EdenFS / Sapling** [18–22] | See below | |
| **rclone mount** [24] | Chunked and parallel reads, read-ahead, sparse cache files | No directory-level prefetch |
| **Egnyte** [25] | Admin "cache warming" of folders; ML-based caching; block-level fetching | Warming aimed at large files |
| **SeaDrive** [28] | Pin or free up space per folder; LRU cache limit | No prefetch documented |
| **Google Drive for desktop** [29] *(snippet)* | Caches recently and frequently used files; offline pinning | |
| **Coda** [30] | Hoard profiles with priorities; a periodic "hoard walk" refetches them | |
| **Academic** [31–32] | Access-graph and successor prediction (Griffioen & Appleton; Kroeger & Long) | |
| **fanotify HSM** (Meta) [33–35] *(snippet)* | Range-carrying `FAN_PRE_ACCESS`; the reference `remote-fetch` has no batching or prefetch | Kernel readahead is disabled while pre-content watches exist; directory (lookup/readdir) events were proposed, not merged |

**Windows Cloud Files API** [1–6]:
- **Hydration policies:** FULL, PROGRESSIVE (complete the request, keep fetching in the background), PARTIAL and
  ALWAYS_FULL.
- **Readahead hint:** fetch callbacks carry an *optional* wider range the provider may fill.
- **Directory population:** FULL, PARTIAL or ALWAYS_FULL enumeration.
- **Pinning:** "Always keep on this device".
- **Explicit hydration:** `CfHydratePlaceholder`.
- **Crawler control:** a per-app toast when an app downloads files in the background, and the user can block that app.

**Apple File Provider** [7–10]:
- **Concurrency:** `NSExtensionFileProviderDownloadPipelineDepth` allows 1–128 concurrent fetches.
- **Per-item content policy:** lazy, lazy-and-evict-on-update, or eager-and-keep.
- **Background prefetch:** `requestDownloadForItemWithIdentifier:` queues a download.
- **Apple DTS advice:** on a partial request, download the whole file and return more than was asked for.

**Meta EdenFS / Sapling** [18–22]:
- **Batching:** fetches from different processes are gathered into batches (`import-batch-size`), with 16 dispatcher
  threads.
- **Sibling prefetch on listing:** `readdir-prefetch` (off by default).
- **Prefetch profiles:** named and predictive (access-history) profiles.
- **Crawler handling:** per-PID fetch counting. Processes above `fetch-heavy-threshold` (default 100k) are
  deprioritized.

## 3. Recommendations for tether, ranked

Constraints: fanotify pre-content events, whole-file hydration at open, Syncthing BEP block requests (which can be
pipelined), a hub server. Already done: the event-bus and index-update fix, the indexer deny list, pinning and the cache
budget.

| # | Technique | Expected impact | Effort | Precedent |
|---|---|---|---|---|
| 1 | **Background fetch scheduler with deep pipelining.** One queue per folder with N outstanding BEP requests to the hub (e.g. 32–64). A blocked open jumps to the front; prefetches fill the rest. Fetch concurrency is independent of how many apps are waiting. | Multiplier for everything below: N files in flight turns N×RTT into about N/depth×RTT. | Medium | Apple pipeline depth, EdenFS batching |
| 2 | **Sibling prefetch.** When a placeholder in directory D is opened, queue the other small placeholders in D (≤ 256 KiB, capped per directory) at low priority. | Very high for grep, IDEs and builds: the 2nd..Nth file of a directory is usually already local when opened. | Low–medium | EdenFS readdir-prefetch, gvfs `--folders`, git checkout batching |
| 3 | **Crawler-aware prefetch.** The event carries the PID. When one process hydrates more than K files in a short window, prefetch that process's subtree ahead of it. Keep denying known indexers, and deprioritize fetch-heavy PIDs rather than deny them. | High for `grep -r`/`rg`; also limits runaway crawls | Medium | EdenFS fetch-heavy handling, Windows per-app blocking |
| 4 | **Eager small files as a folder policy.** Always download files below a threshold (e.g. 16–64 KiB), like pins. They are usually most of the file count but a small share of the bytes. | High for source trees and documents | Low | Apple `downloadEagerlyAndKeepDownloaded`, cfapi pinning (no product publishes a threshold; the numbers are ours) |
| 5 | **Group `fsync`.** When several hydrations complete together, one `syncfs` covers them all instead of one `fsync` each (keep per-file durability semantics). | ~1 ms per file under load | Low | (general technique) |
| 6 | **Multi-file request (BEP extension).** One request, many small files. | Mostly for WAN / high RTT once #1 exists | High (protocol change; stock Syncthing peers would not understand it) | GVFS `/gvfs/objects`, git backfill, Dropbox `retrieve_batch` |
| 7 | **Access-history prediction.** Record co-access per directory and prefetch on first touch. | Medium | High | EdenFS predictive profiles, Coda hoarding |

Suggested order: **1 + 2** first (a scheduler plus sibling prefetch covers most real workloads), then **4** as an
opt-in policy, then **3**. #6 only if WAN use matters.

**Status:** #1 and #2 are implemented (`tether/lib/model/folder_prefetch.go`). The e2e benchmark routes one client
through a 20 ms round-trip-time proxy and runs `grep -r` over 300 small online-only files in 10 directories:

| Prefetch | Time |
|---|---|
| Off | 9.2–9.4 s |
| On | 1.9–2.3 s (4–5×) |

What remains is roughly one full fetch for the first file of each directory, plus files the application reaches
before their prefetch finishes.

**Caveats to design for:**
- **Prefetch costs resources.** It wastes bandwidth and disk, so every prefetch must respect the cache budget and a
  per-directory cap.
- **Crawlers.** Crawlers can hydrate everything: VS/IntelliSense did under GVFS, which is why Windows added its toast.
- **Unbounded hydration.** An unbounded "hydrate folder" can hang the machine (`gvfs prefetch --hydrate`).
- **No directory event.** There is no directory pre-content event yet. Sibling prefetch has to key off the first file
  opened in a directory, or off `FAN_OPEN` on the directory itself. Whether that fires early enough for every tool is
  **unverified**.

## 4. Durability off the critical path (September 2026)

**Measured on the V8 deployment** (Arch client, btrfs `/home`, LAN round trip 0.4 ms): a cold single-file read took
7–16 ms, and a cold `git status` 54.6 s.
- **Why `git status` reads everything:** the stat data in `.git/index` never matches a file created on another
  machine, so git re-hashes every file, and all 19,816 get hydrated.
- **Where the time went:** tracing one hydration showed each `fsync` taking about 9 ms on btrfs (35 fsyncs, 0.31 s,
  for one cold file and its prefetched siblings). Hydration fsynced every file before letting the application go on.

**What others do** (survey; sources below):

| Durability rule | Systems |
|---|---|
| Durable, *then* the "local" flag is published | Syncthing (fsync of the temp file before the rename), Lustre HSM (`llapi_hsm_action_end` fsyncs a restore), GitHub's libprojfs design ("fsync … before removing the empty-placeholder attribute") |
| No fsync; repair after a crash | EdenFS (overlay fsck after an unclean shutdown), CernVM-FS (content-addressed cache, `cvmfs_fsck`), git's default `core.fsync` |
| No fsync, no repair (a flag can outlive its data) | rclone VFS, SeaDrive, Nextcloud |
| Batched durability | git `core.fsyncMethod=batch`, Syncthing's batched directory fsync |

**tether now keeps the invariant but moves the fsync off the application's path:**
- As soon as the content is written, the times are restored and the file is recorded as complete (`hsm.Complete`,
  a flag in the hydration marker). The application goes on.
- A bounded pool (256) makes each file durable in the background, and only then removes the placeholder state and
  the mark (`finishLater`). Until then the file is still a marked placeholder that accesses pass through at once, and
  the scanner leaves it alone.
- **After a crash or power loss,** a file that was not durable yet is an interrupted hydration. It is discarded and
  downloaded again, so it never becomes a local file full of zeros.
- **Explicit hydrations** (pins, `tether hydrate`) still finish before they return.

**The hazard the survey warned about: an application writing inside that window.** The kernel raises the same
pre-content event for reads and writes, so tether cannot tell them apart. The mtime tells it instead: `Complete`
follows restoring the recorded mtime, and any later write changes it.
- `Finish` then leaves the times alone.
- Crash recovery (`Discard`) and a new hydration attempt keep the file as a local change (`ErrModified`) rather than
  discarding it. The scanner then uploads it.
- Tests: `TestWriteAfterComplete`, and e2e `write_while_being_made_durable`, which appends right after the download
  and checks that the server gets the change.

**Bugs the window exposed, found by the V8 stress runs:**
1. **The listener unmarked a file as soon as its handler returned.** A second open inside the window then found an
   unmarked placeholder, and the guard refused it with EIO (17 of 30 evict-then-read cycles failed). The mark now stays
   until the file is durable. Test: e2e `reread_right_after_hydration`, with `TETHER_FINISH_DELAY` holding the window
   open.
2. **The listener's "did the handler finish" check read two xattrs in two calls.** It could see the state before
   and the marker after the finisher removed them. It now reads them in the order they are removed.
3. **The scanner's placeholder check leaked its hydration slot** when the file was "hydrating" but held no slot, which
   the window made common. The next read of that file then waited forever. Test: e2e `scan_while_being_made_durable`,
   which hangs on the old code.
4. **Read-only files** (git objects, 0444): the background `Finish` runs after the file's mode is restored, and
   removing a `user.*` xattr needs write permission. xattr updates now add the owner's write bit for a moment on
   `EACCES`. Test: `TestOpenForWriteReadOnlyFile`, run as `nobody`.
5. **Two hydrations of one file, back to back.** A second caller could check "complete?" (no) just before the first
   hydration completed and released its slot, then take the free slot and hydrate again. That cleared the completion
   flag under the background finisher. The re-check after taking the slot used to be "still a placeholder?", which is
   still true inside the window; it now also checks "complete?". The first hydration marks the file complete before
   it releases the slot.
6. **Folder restarts.** Any configuration change restarts the folder. The slot table, the completion set and the index
   lock lived in the folder instance, so the new instance ignored the old one's hydrations and finishers. They now live
   in the model, per folder ID. Test: e2e `folder_restart_while_being_made_durable`.
   - **Live repro before 5 and 6:** a config change followed by a cold read of the tree failed 2 reads in 1–2 of every
     6 cycles. After: 0 in 12 cycles (about 238,000 reads).
7. **A race introduced by marking before evicting** (`stale-placeholders.md` §4): a reader opening the file between
   the eviction's mark and the placeholder state made the listener remove the fresh mark. The listener now leaves
   inodes being evicted alone.

**Results on V8:**

| | Before | After |
|---|---|---|
| Cold read of a 3.7 KB file | 7.5 ms | 1.7 ms |
| Cold read of a 3.1 KB file (a new directory) | 14 ms | 4–7 ms |
| `cat` of all 199 files of a cold `src/heap` | 197 ms | 88–91 ms |
| Cold read of the whole tree, 19,816 files (`sha256sum -c`) | 55 s | **9.4–15 s**, 0 errors in 28 runs |
| Cold `git status` | 54.6 s | **9.6–15 s** |
| Warm `git status` | 0.5 s | 0.2–0.5 s |

**Where the time goes now.** A CPU profile during a cold read of the whole tree: the daemon uses 2.3 cores.
- About a quarter is `fsync`. It's off the application's path, but CPU-heavy on btrfs.
- About a quarter is SQLite, and 2.1 of 23 CPU seconds went to opening database connections. The folder database
  kept 4 idle connections, and every lookup beyond that opened and closed its own. It now keeps 16 (2 MB page cache
  each).
- One lookup per hydration was repeated (the superseded check); the entry is now reused.

**Measurement notes:**
- **Batched durability doesn't help here.** One `syncfs` per 20 ms batch in place of per-file `fsync` changed nothing
  measurable, and `syncfs` flushes the whole filesystem, which would interfere with the user's other writes. It is
  not used.
- **Runs back to back alternate** between about 9.5 s and 14.5 s. btrfs commits a transaction every 30 s, and a run
  that overlaps the commit of the previous eviction (19,816 truncations) is slower. With 31 s between runs the cold
  read is 9.3–10.2 s with sibling prefetch and 12.9–14.5 s without it, so prefetch pays off on a LAN too.

The remaining per-file cost is the fetch itself, and the first file of each directory still waits for a full round
trip. Crawler-aware prefetch ([docs/design/crawler-prefetch.md](../design/crawler-prefetch.md)) is the next lever.

## Sources

1. https://github.com/MicrosoftDocs/sdk-api/blob/docs/sdk-api-src/content/cfapi/ns-cfapi-cf_sync_policies.md
2. https://github.com/MicrosoftDocs/win32/blob/docs/desktop-src/cfApi/build-a-cloud-file-sync-engine.md
3. https://github.com/MicrosoftDocs/sdk-api/blob/docs/sdk-api-src/content/cfapi/ns-cfapi-cf_callback_parameters.md
4. https://github.com/MicrosoftDocs/sdk-api/blob/docs/sdk-api-src/content/cfapi/nf-cfapi-cfhydrateplaceholder.md
5. https://support.microsoft.com/en-us/windows/automatic-file-download-notifications-in-windows-dc73c9c9-1b4c-a8b7-8d8b-b471736bb5a0
6. https://github.com/MicrosoftDocs/sdk-api/blob/docs/sdk-api-src/content/cfapi/ne-cfapi-cf_hydration_policy_modifier.md
7. https://developer.apple.com/documentation/BundleResources/Information-Property-List/NSExtension/NSExtensionFileProviderDownloadPipelineDepth
8. https://developer.apple.com/documentation/fileprovider/nsfileprovidercontentpolicy
9. https://developer.apple.com/forums/thread/762894
10. https://developer.apple.com/forums/thread/812794
11. https://dropbox.tech/infrastructure/streaming-file-synchronization
12. https://github.com/microsoft/VFSForGit/blob/master/Protocol.md
13. https://learn.microsoft.com/en-us/previous-versions/azure/devops/all/git/gvfs-architecture?view=azure-devops-2020
14. https://github.com/microsoft/VFSForGit/issues/1632 ; https://github.com/Microsoft/VFSForGit/issues/66
15. https://github.com/microsoft/VFSForGit/issues/1347
16. https://git-scm.com/docs/partial-clone
17. https://git-scm.com/docs/git-backfill
18. https://github.com/facebook/sapling/blob/main/eden/fs/docs/Threading.md
19. https://github.com/facebook/sapling/blob/main/eden/fs/config/EdenConfig.h
20. https://github.com/facebook/sapling/blob/main/eden/fs/cli/config.py
21. https://github.com/facebook/sapling/blob/main/eden/fs/store/ObjectStore.cpp
22. EdenConfig.h `store:fetch-heavy-threshold` (same repository as 19)
23. https://cacm.acm.org/research/why-google-stores-billions-of-lines-of-code-in-a-single-repository/
24. https://github.com/rclone/rclone/blob/master/vfs/vfs.md
25. https://helpdesk.egnyte.com/hc/en-us/articles/18140496983181-Cache-Warming-FAQ
26. https://github.com/nextcloud/desktop/issues/7693
27. https://github.com/nextcloud/desktop/issues/10845
28. https://haiwen.github.io/seafile-user-manual/drive_client/drive_client_for_win10/
29. https://support.google.com/drive/answer/13470231?hl=en
30. https://www.cl.cam.ac.uk/teaching/1112/ConcDisSys/DistributedSystems-1B-AdditionalMaterial.pdf
31. https://users.soe.ucsc.edu/~tmk/publications/Usenix96/paper.pdf
32. https://users.soe.ucsc.edu/~tmk/publications/accuracy/accuracy.pdf
33. https://lwn.net/Articles/983376/
34. https://www.phoronix.com/news/Linux-6.14-precontent-fanotify
35. https://github.com/josefbacik/remote-fetch
- EdenFS inode storage: https://github.com/facebook/sapling/blob/main/eden/fs/docs/InodeStorage.md
- EdenFS `FsInodeCatalog.cpp`: https://github.com/facebook/sapling/blob/main/eden/fs/inodes/fscatalog/FsInodeCatalog.cpp
- libprojfs design: https://github.com/github/libprojfs/blob/master/docs/design.md
- Lustre `liblustreapi_hsm.c`: https://github.com/lustre/lustre-release/blob/master/lustre/utils/liblustreapi_hsm.c
- git `core.fsync` and `core.fsyncMethod`: https://git-scm.com/docs/git-config
- CernVM-FS POSIX cache: https://github.com/cvmfs/cvmfs/blob/devel/cvmfs/cache_posix.cc
- rclone VFS cache item: https://github.com/rclone/rclone/blob/master/vfs/vfscache/item.go
- SeaDrive file cache: https://github.com/haiwen/seadrive-fuse/blob/master/src/file-cache-mgr.c
- Syncthing `disableFsync`: https://docs.syncthing.net/users/config.html
