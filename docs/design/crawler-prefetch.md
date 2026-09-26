# Design: crawler-aware prefetch

Status: proposal. Builds on sibling prefetch (`tether/lib/model/folder_prefetch.go`) and the survey in
[`docs/research/small-file-hydration.md`](../research/small-file-hydration.md).

## Problem

Sibling prefetch helps once a tool reaches a directory. Tools that walk a tree depth-first (`grep -r`, `rg`,
`find -exec`, builds, `git status` on a large repo) still pay one full fetch for the first file they open in *every*
directory. In trees with one or a few files per directory, that is most of the cost.

Goal: recognise a process that is walking the tree, and fetch ahead of it.

## 1. Signals

- **Placeholder opens.** `FAN_OPEN_PERM` for every placeholder opened through the view. We already receive these; the
  event carries the PID, from which we get the executable, working directory, process group and cgroup.
- **Directory opens (new).** Add the notification `FAN_OPEN | FAN_ONDIR` to the view's mark. A crawler opens a
  directory *before* opening the files in it, so this is the earliest signal. It is an asynchronous notification
  with no response required, so it is cheap. **To verify first:** that the kernel accepts it on a mount mark in our
  `FAN_CLASS_PRE_CONTENT` group, on 6.14 and on ≥ 6.17.
- **Local files are invisible.** Local files carry evictable ignore marks, so we don't see opens of them. That is
  fine: only placeholders matter.

## 2. Actors

A crawl is often many short processes; `find -exec grep` starts one `grep` per file. Statistics are therefore kept
per **actor**: the process group, falling back to the cgroup (e.g. a systemd scope per desktop app).

Each actor has a sliding-window record:

| Field | |
|---|---|
| Placeholders hydrated | count |
| Directories opened | set |
| Bytes pulled | total |
| Last activity | timestamp |

Actors expire after a few seconds of inactivity. Their queued work is dropped; in-flight downloads finish.

## 3. Detection

An actor is a **crawler** when, within about 2 s, it hydrates at least K placeholders (K ≈ 8) across at least 2
directories.

- **Hydrations are required.** Directory opens alone never qualify. `find`, `ls -R` and `du` walk entire trees
  without reading content, and must not cause downloads.
- **Known tools may use a lower K:** `grep`, `rg`, `ag`, `make`, `cc`, `git`, `tar`, `rsync`.
- **Indexers stay denied:** the existing hydration deny list.

## 4. Prediction

- **Follow the walk.** When a crawler opens directory D, schedule the placeholders below D, depth-first.
- **Use the crawler's own order.** List the directory through the *lower* (unmarked) path with `getdents64`. The
  kernel returns entries of the same directory in the same order to every reader, so the prediction matches the
  crawler's actual order, and it stays filesystem-agnostic.
- **Lookahead window.** Stay W files / B bytes ahead of the crawler's position:
  - Grow W while the crawler's demand opens hit files that are queued but not yet fetched.
  - Shrink W when prefetched files go unused, for example subtrees skipped by `--exclude-dir` or `.gitignore`.
- **Size limit.** Higher than for siblings (1–4 MiB), since crawlers usually read whole files. Larger files stay on
  demand.

## 5. Scheduling

The prefetch FIFO becomes a priority queue with three classes:

1. **Demand:** an application is blocked on this file right now.
2. **Crawler lookahead:** one lane per crawler, served round-robin; nearest-first within a lane.
3. **Sibling prefetch:** today's behaviour.

Unchanged:
- one download per name, which blocked opens join
- prefetched files get an old atime, so the cache budget evicts unused ones first
- index updates are batched off the application's path

## 6. Guardrails

The survey's cautionary tales: VS IntelliSense hydrating a whole VFS for Git repo, and EdenFS's fetch-heavy handling.

- **Per-actor byte cap.** For example 1 GiB, or a fraction of the cache budget. Beyond it we stop speculating; demand
  opens are still served.
- **Notify the user** over D-Bus when a cap is hit ("grep is downloading ~/Docs"), like Windows' per-app download
  notification.
- **Deprioritize fetch-heavy actors.** Above a much higher threshold, an actor's *own* demand requests drop below
  other applications', so interactive apps stay responsive during a large crawl.
- **Cache budget.** With a budget set, prefetched-but-unconsumed bytes stay below a small fraction of it, so a crawl
  cannot flush the cache.

## 7. Implementation sketch

| Where | Change |
|---|---|
| `lib/hsm` | Add `FAN_OPEN\|FAN_ONDIR` to the mark. Pass PID and executable to the handler. Add a `DirOpened(folder, dir, pid)` callback. |
| `lib/model/folder_prefetch.go` | Actor table, detector, lookahead controller, priority queue with per-actor lanes, cancellation on actor expiry |
| config | `crawlerPrefetch` (on/off), `crawlerMaxBytes`, lookahead limits, crawler file-size limit |
| REST / CLI | List active crawlers and what they have pulled (`tether status`) |

## 8. Tests

- **Deep tree, one file per directory, 20 ms RTT (`e2e/latproxy`).** `grep -r` over it, where sibling prefetch alone
  gives nothing. Expect an N-fold improvement, bounded by bandwidth rather than round trips.
- **Metadata-only walks** (`find`, `du -a`) over placeholders hydrate nothing.
- **Byte cap** stops speculation; content stays correct.
- **Interactive latency.** An interactive `cat` stays fast while a large crawl runs.
- **Cancellation.** Killing the crawler cancels its queued prefetches.
- **Fairness.** Two concurrent crawlers share the workers.

## Open questions

- Whether `FAN_OPEN|FAN_ONDIR` works on a mount mark in the pre-content group, and on which kernels. This is the first
  thing to check, since everything else depends on it.
- The best actor key: process group for shells and `find -exec`, cgroup for IDEs and other multi-process apps.
- Tools that sort entries themselves (`rg --sort`) or walk with parallel threads (`rg` by default) won't follow
  `getdents` order exactly. The adaptive window absorbs some of that; measure it.
