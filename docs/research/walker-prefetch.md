# Prefetching for walkers that don't follow directory order

Prefetch ahead of tree walks ([docs/design/crawler-prefetch.md](../design/crawler-prefetch.md)) first assumed that a
walker visits entries in the order the filesystem lists them. That holds for `find` and `grep -r`. Measuring other
walkers found three problems:

1. **Sorted walkers.** `rg --sort path`, and `git status`, which re-reads files in index order, visit entries by name.
2. **Parallel walkers.** `rg` walks with several threads by default, so it is at several places at once.
3. **Filtering walkers.** `grep -r --include=*.c`, `rg -t`, `find -name` open only some of the files.

This note covers the prior art we found and what tether does as a result.

## Measurements before

Setup: Docker cluster (4 cores), 20 ms RTT through `e2e/latproxy`, 600 files of 600 B, one file per directory
(`git`: 400 files). "Off" means sibling prefetch only.

| Walker | Off | Directory-order prediction |
|---|---|---|
| `grep -r` | 15.8 s | 1.1 s |
| `find -exec cat {} +` | 17.3 s | 1.2 s |
| `rg` (threads) | 7.8 s | 3.4 s |
| `rg --sort path` | 17.3 s | 15.0 s |
| `git status` (cold, re-hashes every file) | 12.9 s | 7.5 s |
| `grep -r --include=*.c` (1 file in 4 matches) | 5.8 s | 1.1 s, but 439 of 450 non-matching files downloaded |

## Prior art

Egress to usenix.org, kernel.org, arxiv.org and several paper sites was blocked for the survey. Paper details marked
(unverified) come from abstracts or search snippets. The EdenFS and Linux sources were read directly.

### Sorted walkers

- **EdenFS walk detector** (Sapling, `eden/scm/lib/walkdetector`, [1], [2]). The closest prior art. It does not
  predict an order at all:
  - **Detection:** a directory counts as walked once 3 of its children have been accessed (at least 3% of its entries
    when the size is known).
  - **Growing upward:** walks in sibling directories merge into one walk of the parent, and the whole subtree is then
    fetched in batches of 4096.
  - **Expiry:** walks expire after 5 s without access. Accesses served from the cache keep a walk alive.
- **Linux readahead** [3], [4]. Keeps no per-stream order. It confirms sequential access from the page cache: are
  the preceding pages cached?
  - **Ramp-up:** 4× then 2× the previous window, up to a maximum.
  - **Next window:** a marker page starts the next asynchronous window when the reader reaches it.
- **VFS for Git / Scalar** [6]. Prefetches commits and trees, and fetches blobs on demand. It does not predict
  walkers.

### Parallel walkers

These come from hardware and storage prefetchers:
- **Stream tables.** IBM POWER keeps at least 16 entries, replaced round-robin [7]. Intel's L2 streamer keeps 32
  streams [8]. Jouppi's stream buffers replace the least recently hit buffer [9].
- **New streams:** allocated on a miss and confirmed by the next accesses in sequence.
- **AMP** (FAST '07) [10] adapts each stream's prefetch degree:
  - grows it when prefetched data arrives late
  - shrinks it when data is evicted unused (unverified)

### Filtering walkers and accuracy

- **Feedback-directed prefetching** (Srinath et al., HPCA '07) [11] measures accuracy, lateness and pollution per
  interval. It raises aggressiveness when prefetches are accurate but late, and lowers it when accuracy is low
  (thresholds of 0.75 / 0.40 are reported; unverified).
- **EdenFS** pauses a walk's prefetch when its hits ÷ prefetched fall below 0.1, once more than a configured lag
  (`max_initial_lag`) of prefetched files are unused.
- **EdenFS's predictive prefetch profiles** learn up to 1500 globs from access logs. We found no published online
  learning of a scanner's `--include` filter.
- **Cloud clients** don't predict across files:
  - Windows Cloud Files (OneDrive and others) only lets the user block an app from hydrating [12], [13].
  - rclone reads ahead within a file [14].
  - CernVM-FS relies on explicit preloading [15].
  - Nothing public was found for Dropbox or iCloud.

## What tether does

Each change is followed by the reason for it.

1. **Two orders, chosen by mispredictions.** A lane starts in directory order. When the walker opens a placeholder
   we didn't predict, we check whether each order explains it: is it among the 32 files after the walker's last
   known position in that order? Whichever order explains it is used from then on. Sorted order compares names
   bytewise, which is also git's index order for these trees.
   - This is the cheap form of keeping candidate orders side by side (Linux readahead confirms from the cache in the
     same spirit). Only mispredictions pay for the check.
2. **Sweeping for walkers with no order.**
   - **When:** as soon as a walker has two placeholder opens waiting at once, or when mispredictions that neither
     order explains reach 3 and at least 1 in 8 of its opens.
   - **What:** fetch the whole subtree from its start, as EdenFS does. Its window is 1024 files, and it pauses while
     fewer than 1 in 10 fetched files have been opened after 1000 unopened (EdenFS's rule).
   - **Why two opens:** a process that waits for one open at a time never has two waiting, so two is a direct sign
     of threads or several processes. This replaces a stream table: without an order, several stream positions
     predict no better than the subtree does.
   - **Why the share:** git opens a few objects and `.git/info/exclude` after walking the work tree, and should keep
     its order.
   - **Why no widening:** a sweep doesn't widen when it finishes, because without an order, finishing the subtree
     says nothing about where the walker goes next. Before this rule, one sweep went on into unrelated trees in the
     folder.
3. **Learning what a walker skips.** In an ordered lane, a predicted file 8 positions behind the walker counts as
   passed by. Per file extension and per directory name, we count files passed by and files opened.
   - **Exclusion:** once 8 files of a kind have been passed by and none opened, we stop predicting that kind, and
     queued files of that kind are dropped. An open of such a file lifts the exclusion.
   - **Examples:** this learns `--include=*.c`, `rg -t`, and walkers skipping `.git` or `node_modules`.
   - **Resets:** the counts reset when the order changes. Files seemingly passed by under the wrong order
     previously excluded whole directory names (`e05` under every `dNN`). Counts are keyed on names, not paths
     relative to the root, which can widen.
   - **Accuracy feedback:** the window shrinks for every file passed by, as in feedback-directed prefetching and
     AMP.
4. **Ramp-up.** The first window is 16 files (it was 64). It doubles each time the walker waits for a predicted file
   that is still downloading, as in readahead and AMP ramp-up. A filtering walker then costs fewer speculative files
   before its filter is learned.

## Measurements after

Same setup, 600 files:

| Walker | Off | On |
|---|---|---|
| `grep -r` | 17.6 s | 1.4 s |
| `find -exec cat {} +` | 17.7 s | 1.2 s |
| `rg` (threads) | 7.7 s | 1.1 s |
| `rg --sort path` | 17.7 s | 1.3 s |
| `git status` | 12.9 s | 1.4 s |
| `grep -r --include=*.c` | 5.8 s | 0.9 s, 27 of 450 non-matching files downloaded |

The e2e test `crawl_prefetch_other_walkers` uses 200-file trees (600 for `--include`) and requires each walker to be
at least 2× faster. In two consecutive runs:

| Walker | Speedup |
|---|---|
| `rg` | 3.0–3.2× |
| `rg --sort path` | 7.3–7.4× |
| `git status` | 5.4–6.6× |
| `--include` | 6.8×, 27–46 non-matching files fetched |

Also found while measuring:
- **Harness race.** The e2e harness could leave a device connected after pausing it, when a connection was dialled
  as the pause took effect (upstream Syncthing). The harness now pauses again.
- **`git status` writes.** It rewrites `.git/index` on a client, which then syncs to the hub like any local change.

## Sources

1. https://github.com/facebook/sapling/tree/main/eden/scm/lib/walkdetector (and `eden/scm/lib/backingstore/src/prefetch.rs`)
2. https://github.com/facebook/sapling/blob/main/eden/fs/config/EdenConfig.h
3. https://lkml.iu.edu/hypermail/linux/kernel/0904.1/01648.html
4. https://github.com/torvalds/linux/blob/master/mm/readahead.c
5. https://postgrespro.com/list/id/f3xxfrkafjxpyqxywcxricxgyizjirfceychyxsgn7bwjp5eda@kwbduhy7tfmu
6. https://github.com/microsoft/VFSForGit/blob/master/Protocol.md
7. US patent 7958317 (IBM stream prefetch)
8. https://cdrdv2-public.intel.com/795247/357930-Hardware-Prefetch-Controls-for-Intel-Atom-Cores.pdf
9. US patent 5371870 (Jouppi, stream buffers)
10. https://www.usenix.org/conference/fast-07/amp-adaptive-multi-stream-prefetching-shared-cache
11. https://dl.acm.org/doi/10.1109/hpca.2007.346185
12. https://github.com/MicrosoftDocs/win32/blob/docs/desktop-src/cfApi/build-a-cloud-file-sync-engine.md
13. https://learn.microsoft.com/en-us/windows/win32/api/cfapi/ne-cfapi-cf_hydration_policy_modifier
14. https://github.com/rclone/rclone/blob/master/vfs/vfs.md
15. https://cvmfs.readthedocs.io/en/latest/cpt-hpc.html
