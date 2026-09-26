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
