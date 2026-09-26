# Design: crawler-aware prefetch

Status: **implemented** (`tether/lib/model/folder_crawl.go`), except for the guardrails listed under
[Not implemented](#not-implemented). Sorted, parallel and filtering walkers, with their prior art and measurements, are
covered in [docs/research/walker-prefetch.md](../research/walker-prefetch.md). Builds on sibling prefetch (`tether/lib/model/folder_prefetch.go`) and the survey
in [`docs/research/small-file-hydration.md`](../research/small-file-hydration.md).

## Problem

Sibling prefetch helps once a tool reaches a directory. Tools that walk a tree depth-first (`grep -r`, `find -exec`,
builds) still wait one full fetch for the first file they open in *every* directory. In trees with one or a few files
per directory, that is most of the cost.

Goal: recognise a process that is walking the tree, and fetch ahead of it.

## Results

`grep -r` over online-only trees with one file per directory, three-node Docker cluster on one 4-core host,
`e2e/latproxy` for the latency. "Off" still has sibling prefetch; it has nothing to do in these trees.

| Tree | Link | Off | On | |
|---|---|---|---|---|
| 120 directories | 20 ms RTT | 3.9 s | 1.1 s | 3.7× (e2e `crawl_prefetch_under_latency`) |
| 600 directories | 20 ms RTT | 17.4 s | 1.1 s | 15× (2.4 s with 16 prefetch workers) |
| 600 directories | LAN (Docker bridge) | 2.0 s | 0.53 s | 3.8× |

Other walkers (600 files, 20 ms RTT; details in the research note):

| Walker | Off | On |
|---|---|---|
| `rg` (threads) | 7.7 s | 1.1 s |
| `rg --sort path` | 17.7 s | 1.3 s |
| `git status` (cold, 400 files) | 12.9 s | 1.4 s |
| `grep -r --include=*.c` | 5.8 s | 0.9 s; 27 of the 450 files it skips were fetched |

The prediction was exact for GNU grep over 600 files: it opened 4 cold before being recognised, and every one of the
other 596 had been predicted (no mispredictions). With 16 workers the 20 ms case was bound by concurrency
(about 16 files per round trip), so `prefetchConcurrency` now defaults to 64. On the LAN the host's CPUs are the limit.

## 1. Signals

We learn about the walker from two events, both of which carry the accessing process ID:

- **Placeholder opens:** the `FAN_OPEN_PERM` event of every placeholder opened, through any path. The listener passes
  the PID to the handler (`hsm.Accessor(ctx)`).
- **First opens of prefetched files.** A file downloaded ahead of a walker stays marked after its download instead of
  being unmarked. Its first open raises one more `FAN_OPEN_PERM`. The listener allows it at once, removes the mark and
  reports it (`hsm.OpenObserver`). This tells us where the walker is even when every file it opens is already local.
  The cost is one event per prefetched file, which is microseconds against a round trip.

**Why not directory opens?** The earlier plan used `FAN_OPEN | FAN_ONDIR` on the view's mount mark. Since then tether
marks each placeholder's inode instead of the view's mount (so containers and sandboxes download too). Directory opens
would need a mark on every directory. The kept marks give a better signal anyway: they report the walker's actual
progress, not the directories it lists.

## 2. Actors

A walk is often many short processes; `find -exec grep` starts one `grep` per file. Walks are therefore tracked per
**process group** (`getpgid`, falling back to the PID once the process has exited). The detector keeps one record per
group, and at most 64 of them.

An actor is forgotten when its process group has no members left (checked every second), or after 30 s without
opening anything we prefetched or predicted. Its files still queued are dropped, and kept marks on files it never
opened are removed. Downloads already in flight finish.

## 3. Detection

An actor is a **walker** when it opens placeholders in at least **4 directories within 2 s**.

- **Placeholder opens only.** `find`, `ls -R`, `du` and `stat` never open file content and never qualify. The e2e test
  checks that such walks download nothing.
- **Indexers never qualify:** they are denied before any hydration (the existing deny list).
- **The root** of the walk is taken to be the deepest directory containing those first directories.
- **Near the folder root, more evidence.** If those directories have nothing in common below the folder root, the
  actor must open placeholders in 8 directories, not 4. A mistaken walk of the whole folder costs the most; EdenFS
  is stricter near the repository root for the same reason.

## 4. Prediction

- **Follow the walk.** Prediction is a depth-first walk below the root, starting after the file just opened. It lists
  directories through the data directory (`path`) in one of two orders:
  - **Directory order,** as the filesystem returns entries (`getdents`, unsorted). This is the order `find` and
    `grep -r` use, because every reader of a directory gets the same order. It is filesystem-agnostic.
  - **Sorted by name,** as `rg --sort path` and git (index order) visit files.

  Only placeholders up to `crawlPrefetchMaxFileKiB` (default 1024) are queued.
- **Mispredictions.** A walker opening a placeholder we had not predicted moves the prediction there. If it is outside
  the root, the root widens to the common ancestor. Each misprediction also tests both orders: the order that has
  the file among the 32 after the walker's last position is used from then on.
- **Walkers without an order.** Some walkers have no order we can follow. We sweep the whole subtree from its start
  (as EdenFS does) instead of following a position, and pause while fewer than 1 in 10 files fetched this far were
  opened. This happens in two cases:
  - **Two opens waiting at once.** Several threads or processes are walking, e.g. `rg` or `make -j`. A process waiting
    for one open at a time never has two.
  - **Unexplained mispredictions:** at least 3 that neither order explains, making up at least 1 in 8 of the walker's
    opens.
- **Learning what the walker skips.** A predicted file 8 positions behind the walker counts as passed by.
  - **Exclusion:** once 8 files with the same extension, or in directories with the same name, have been passed by and
    none opened, files like them are no longer predicted, and queued ones are dropped. This handles
    `grep --include=*.c`, `rg -t`, and walkers skipping `.git`.
  - **Lifting it:** an open of such a file lifts the exclusion.
  - **Resets:** the counts reset when the order changes.
- **Past the root.** When the walk below the root is done, the root widens to its parent and the prediction continues
  after the old root, with the smallest window. The walker's real root is not known (we only see where it started),
  so going on costs at most a few speculative files if it has stopped.
  - **Never into the whole folder.** Speculation doesn't widen to the folder root; a walker that goes on there shows
    it by opening a placeholder outside.
  - **Sweeps never widen** by themselves.
- **Window.** At most W files are downloaded ahead of the walker and not yet opened by it. W starts at 16, stays
  between 16 and 1024 (a sweep uses 1024), and adapts:
  - **Grows:** doubles whenever the walker opens a predicted file that is still downloading.
  - **Shrinks:** decreases by one for every predicted file the walker passes without opening, i.e. once it has opened
    a file 8 positions further on.
- **Bytes.** At most 256 MiB, or a tenth of a fixed `cacheBudget` if that is smaller, may be downloaded ahead and not
  yet opened, and at most `crawlPrefetchMaxMiB` (default 1024) per walk in total. Beyond that we stop speculating and
  say so in the log; the walker's own opens are still served.
- **Eviction feedback.** When the cache budget evicts a file prefetched for a walker that never opened it, that
  walker's window halves. Its speculation is displacing cached files (AMP shrinks a stream's prefetch degree the same
  way).
- **Sweep accuracy, stricter near the root.** A sweep pauses while fewer than 1 in 10 of its files were opened. The
  pause starts once 1000 are unopened, but 250 when the sweep's root is a top-level directory, and 64 at the folder
  root.

## 5. Scheduling

- The prefetch queue has two classes: siblings of opened files first (what the application needs next), then one
  lane per walker, served round-robin.
- Opens by applications never go through the queue, so they are never behind speculation.
- Unchanged:
  - one download per file, which blocked opens join
  - prefetched files get an old atime, so the cache budget evicts unused ones first (relatime refreshes it on the
    first real read)
  - index updates are batched off the application's path

## Not implemented

From the survey's cautionary tales (VS IntelliSense hydrating a whole VFS for Git repo; EdenFS's fetch-heavy
handling):

- **Notifying the user** over D-Bus when a walker hits its byte cap, like Windows' per-app download notification.
  Today this is a log line.
- **Deprioritising fetch-heavy actors'** own demand opens below other applications'.

**Reporting.** Active walkers, and the last 16 that ended, are reported by `tether walkers [PATH]` and
`GET /rest/ondemand/walkers?folder=`. Each entry gives:
- program, process group and root
- order
- files opened, prefetched and evicted unopened
- bytes
- whether it reached its cap
- the extensions and directory names it was learned to skip

## Tests

- **Unit:** `lib/model/folder_crawl_test.go` checks that the prediction matches `find`'s order from any starting
  file, handles roots and vanished files, and checks the queue's classes and lanes. `lib/hsm`: the accessor PID, the
  kept-mark report, and that unmarking unused files never unmarks a placeholder.
- **e2e `crawl_prefetch_under_latency`:**
  - a tree of one file per directory at 20 ms RTT, with and without; must be at least 2× faster
  - `find`, `du -a` and `ls -lR` first, which must download nothing
  - contents checked
- **e2e `crawl_prefetch_other_walkers`:** `grep -r --include=*.c` (at most a quarter of the skipped files fetched),
  `rg`, `rg --sort path` and `git status`, each at least 2× faster at 20 ms RTT. `rg` and `git` are copied from the
  host into the containers (`e2e/run.sh`); those cases are skipped without them.
- **e2e `crawl_prefetch_byte_cap`:** a process that opens files in four directories and then idles costs exactly
  `crawlPrefetchMaxMiB` of lookahead.

## Open questions

- **Stream tables.** Parallel walkers are handled by sweeping the subtree. A table of several streams, each with its
  own position, as hardware prefetchers keep, would fetch in a better order for large trees with several walker
  threads, but has not been needed at the sizes measured.
- **Exclusions by directory name** generalise across the tree (e.g. `.git`, `node_modules`, `build`). A false
  exclusion costs one fetch per affected name, when the walker opens such a file and lifts it.
- **Not yet measured on the V8 deployment:** cold `git status` there took 10–15 s with sibling prefetch alone.
