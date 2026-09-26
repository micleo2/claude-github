# Design: crawler-aware prefetch

Status: **implemented** (`tether/lib/model/folder_crawl.go`), except for the guardrails listed under
[Not implemented](#not-implemented). Builds on sibling prefetch (`tether/lib/model/folder_prefetch.go`) and the survey
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

## 4. Prediction

- **Follow the walk.** Prediction is a depth-first walk below the root, starting after the file just opened. It lists
  directories through the data directory (`path`) in the order the filesystem returns entries (`getdents`, unsorted).
  That is the order in which `find` and `grep -r` visit a directory, because every reader of a directory gets the same
  order. It is filesystem-agnostic. Only placeholders up to `crawlPrefetchMaxFileKiB` (default 1024) are queued.
- **Mispredictions.** A walker opening a placeholder we had not predicted moves the prediction there. If it is outside
  the root, the root widens to the common ancestor.
- **Past the root.** When the walk below the root is done, the root widens to its parent and the prediction continues
  after the old root, with the smallest window. The walker's real root is not known (we only see where it started),
  so going on costs at most a few speculative files if it has stopped.
- **Window.** At most W files are downloaded ahead of the walker and not yet opened by it. W starts at 64, stays
  between 16 and 1024, and adapts:
  - **Grows:** doubles whenever the walker opens a predicted file that is still downloading.
  - **Shrinks:** decreases by one for every predicted file the walker passes without opening, i.e. once it has opened
    a file 64 positions further on. The slack allows for tools that sort the entries of a directory themselves.
- **Bytes.** At most 256 MiB, or a tenth of a fixed `cacheBudget` if that is smaller, may be downloaded ahead and not
  yet opened, and at most `crawlPrefetchMaxMiB` (default 1024) per walk in total. Beyond that we stop speculating and
  say so in the log; the walker's own opens are still served.

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
- **Reporting active walkers** in `tether status` / REST. They are in the debug log for now.

## Tests

- **Unit:** `lib/model/folder_crawl_test.go` checks that the prediction matches `find`'s order from any starting
  file, handles roots and vanished files, and checks the queue's classes and lanes. `lib/hsm`: the accessor PID, the
  kept-mark report, and that unmarking unused files never unmarks a placeholder.
- **e2e `crawl_prefetch_under_latency`:**
  - a tree of one file per directory at 20 ms RTT, with and without; must be at least 2× faster
  - `find`, `du -a` and `ls -lR` first, which must download nothing
  - contents checked
- **e2e `crawl_prefetch_byte_cap`:** a process that opens files in four directories and then idles costs exactly
  `crawlPrefetchMaxMiB` of lookahead.

## Open questions

- **Parallel walkers.** `rg` walks with several threads by default, and `git status` reads files in index (sorted)
  order from several threads. Prediction then leads by directory rather than by file. Mispredictions reposition it,
  and the window absorbs some reordering, but this is not measured yet (neither tool is in the e2e image).
- **Filtering walkers** (`grep -r --include=*.c`) never open most predicted files. The window shrinks to 16 but does
  not stop, so up to `crawlPrefetchMaxMiB` of files the walker skips may be downloaded. Learning the filter, for
  example by extension, would bound this better.
