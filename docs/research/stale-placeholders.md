# Stale placeholders and failed hydrations: survey and what tether does

A placeholder stands for one version of a file: its xattrs record the block list hash of that version. Two things can
go wrong between creating a placeholder and reading it:

- **The version is superseded.** Another device changes the file, the index update arrives, but the puller has not
  yet replaced the placeholder. A read then asks peers for blocks of a version they no longer have.
- **A hydration fails halfway**, because a peer disconnects, a block is gone, or the daemon crashes. Some blocks are
  already written into the placeholder.

## 1. What happened on the V8 deployment (September 2026)

Setup: the V8 tree, server on Debian, on-demand client on Arch.

1. Running `git status` on the server rewrote `.git/index` (2.1 MB).
2. Seconds later, the client evicted everything and then read every file through the view.

The read of `.git/index` asked the server for the old version's blocks. The server had some of them and failed at
offset 655,360 ("no such file"), so the application got EIO. So far that was only unhelpful, but it got worse:

1. **The failed hydration left the mtime bumped.** Writing the first five blocks changed the placeholder's modification
   time, and only a successful hydration reset it.
2. **The puller refused to replace the placeholder.** It compares size and mtime with the index before replacing a
   file: "file modified but not rescanned". It asked for a rescan.
3. **The scanner announced a new version.** It saw a placeholder whose mtime differed from the index and took it for
   a metadata change of a placeholder, and it announced a new version of the *old* content. That version was
   concurrent with the server's real change and had the newer mtime, so it would have won the conflict. The server's
   actual `.git/index` would have become a `.sync-conflict` copy. This was stopped only because nobody had the old
   blocks any more; the server's pull kept failing.

So a failed download turned into a conflict that the stale side would have won. Both root causes needed fixing:
switching to the new version, and restoring placeholders after failed hydrations.

## 2. What other systems do

| System | Placeholder pinned to | Remote version changed before hydration | Failure after partial download |
|---|---|---|---|
| **Windows Cloud Files** | `FileIdentity` blob | `CF_OPERATION_TYPE_RESTART_HYDRATION`: the placeholder is completely dehydrated, gets new metadata (size, times) and identity, and "pending user IO requests… will be reprocessed as if they just arrived". The docs name exactly this case: "the file contents have actually been updated in the cloud (and the sync provider is not able to retrieve historical contents)". | Fail the transfer with `STATUS_CLOUD_FILE_*`. `CfUpdatePlaceholder` "will fail rather than result in torn file contents". |
| **Apple File Provider** | `contentVersion` | With no version requested, `fetchContents` may return "the same or newer" version with its item. With `.strictVersioning` it returns `versionNoLongerAvailable` instead. | `fetchPartialContents` uses `.strictVersioning` once content exists "to prevent the system from accidentally writing mismatched pieces from different versions to the same file". |
| **mountpoint-s3** | ETag at open | Every range GET carries `If-Match`. A 412 becomes `ESTALE`; reopening gives the new version. Reads return "either the old data or the new data, but never partial or corrupt data". | The cache is keyed by ETag, so blocks are never mixed. |
| **rclone VFS** | size, modtime, hash | At open, a changed remote fingerprint with no local changes drops the cache item, including partial ranges, and serves the latest. | The docs admit a remote change during a read can corrupt it. |
| **SeaDrive** | file ID | A changed ID with an unmodified cache: the file is marked OUTDATED, truncated and refetched. With a modified cache: kept, conflict. | An interrupted fetch of the same ID resumes. A server error deletes the partial file. |
| **Nextcloud (cfapi)** | none | An unconditional GET of the current path serves the latest. | A cancelled hydration deletes the partial file and recreates the placeholder. The index stays "virtual". |
| **EdenFS, VFS for Git, CernVM-FS** | content hash | Content-addressed and immutable: the old version is always fetchable. A new version arrives only by updating the placeholder. | Not applicable, or retried against the same hash. |
| **Microsoft CloudMirror sample, goofys** | none | Serves whatever is current, unchecked: torn files are possible. | Same. |

**The common pattern:**
- Pin every placeholder to an immutable version, and check every fetched range against the pin.
- **Nothing materialised yet:** switch to the latest version, but update the placeholder's metadata first, atomically.
  Examples: cfapi `RESTART_HYDRATION`, File Provider without strict versioning, rclone and SeaDrive at open.
- **Some bytes already exposed:** fail with a distinct error instead of splicing versions (Apple, mountpoint-s3).
- **A failed or interrupted hydration** leaves the placeholder as it was: partial ranges are dropped and the metadata
  restored (cfapi, Nextcloud, rclone).

The failure path never leaves anything behind that looks like a local change.

## 3. What tether does now

tether already pins placeholders: the `user.tether.bh` xattr is the block list hash, and every fetched block is
checked against its SHA-256. tether also never exposes a byte of a placeholder before it is completely hydrated: the
open, or the first read, waits for the whole file. So tether is always in the "nothing materialised" case, and
switching versions is always safe.

- **Superseded placeholders are hydrated at the latest version** (`supersededBy` and `switchPlaceholder`). Before
  fetching, and again after a failed fetch, tether checks whether the global version replaces the placeholder's. It
  switches when all of these hold:
  - the placeholder is the local version, unmodified (still virtual);
  - the global version is strictly newer by version vector;
  - the permissions match;
  - the placeholder is still the file at that name.

  The switch changes the placeholder in place (size, identity, mtime) and records it in the index as the new local
  version, as the puller would have. Whether the content then arrives or not, the index describes what is on disk. The
  hydration itself is recorded in the usual batch.
  - The application's open file sees the new content, as with cfapi's reprocessed I/O.
  - If the global version is a deletion, the read fails and the puller removes the file.
- **A peer that no longer has the content is given a moment.** A peer that is asked for blocks that no longer match
  its file answers "no such file" and rescans that file at once. The read waits up to 5 s for the new version to be
  announced, then switches to it. The wait is bounded because there may be no newer version at all: the peer's copy
  may have lost a conflict.
- **A placeholder that was replaced while being opened** (the reader holds the old, unlinked inode) is hydrated at the
  latest version. Nothing is recorded, because no name refers to it.
- **The puller's batch is flushed on demand.** The puller records placeholders it created in batches, up to 2 s late.
  A read that finds a placeholder the index does not know yet asks the puller to commit now, and waits for it.
- **A failed hydration restores the placeholder** (`hsm.BeginHydration` and `hsm.Discard`).
  - Before the first write, the `user.tether.hydrating` xattr records the placeholder's mtime.
  - On failure, the content is freed (truncated to zero, then back to size) and the mtime restored from the xattr. A
    placeholder with data blocks would also get past the guard's fast path.
  - `Finish` removes the xattr last, after the placeholder state.
  - A later attempt keeps an existing xattr and finishes with the time recorded in it, so a retry that succeeds
    before the scanner gets there also restores the right mtime.
- **Interrupted hydrations are repaired by the scanner.** A crash can leave the xattr behind. The scanner's placeholder
  check sees it and discards the partial content before looking at the file.
- **The scanner never sees a mid-hydration mtime.** The placeholder check claims the file's hydration slot (the same
  one concurrent hydrations share) while it stats the file. The walk passes the size and mtime taken there to the
  scanner, not what it saw earlier. A hydration that is running makes the scanner leave the file alone.
- **Our index updates are ordered with the puller's.** One mutex covers the puller's batch commit and each of our
  commits, from checking the disk to writing: hydrations, switches and evictions.
  - **The race it closes:** one of our updates validated against the disk just before the puller replaced the file,
    and landed after the puller's record of the replacement. The index then described a file that was gone.
  - **Eviction batches** are re-validated against the index and the disk at commit time.
- **Placeholders the index does not know heal themselves.** A placeholder can stand for content that neither the local
  nor the global index entry knows, for 10 s, while the local entry says there is no content. The scanner then resets
  it to the local entry. Nothing is lost, because neither side has content. Before this, such a file was stuck for
  good: the scanner leaves unknown placeholders alone, and the puller refuses to replace a file that differs from the
  index.
- **A crash between switching and recording is recoverable.** If the scanner finds a placeholder that matches a
  strictly newer global version, it records that version instead of minting a new, conflicting one.

Tests:
- e2e `superseded_placeholder_hydrates_latest`: the puller is held back with `pullerDelayS`, the server replaces the
  file, and a read gets the new content, with no conflict on either side.
- e2e `interrupted_hydration_is_reset`: a leftover marker plus a bumped mtime are reset, and no new version is
  announced.
- `hsm` `TestDiscardAndRetarget`.

Both e2e tests fail on the previous build.

## 4. Churn stress test (V8 deployment)

- **Server:** 150 self-verifying files (3 KB to 1.1 MB; each carries a SHA-256 of itself) in the V8 tree, one
  rewritten atomically every 50 ms for 200 s. That is 3,690 writes, and each file changes about every 7.5 s.
- **Client:** four readers check random files through the view, and the whole directory is evicted every 3 s, for
  170 s. A read counts as *bad* if it returns anything but one complete, valid version.

| Build | Reads OK | Bad | Failed (EIO) | Other findings |
|---|---|---|---|---|
| Before this work | 339,962 | 0 | 96,768 | "index no longer knows the content" on almost every read of a changed file. 47 files stuck for good ("file modified but not rescanned"). |
| + switching and restoring | 4,380 | **1 (all zeros)** | 2,539 | Unknown placeholders are still created and healed (57) |
| + mark before lease (below) | 2,188 | 0 | 212 | 80 unknown placeholders healed |
| + switch recorded at once, 5 s wait | **31,975** | **0** | **55** (0.2%) | None: no unknown placeholders, no stuck files |

The first row's high OK count is mostly repeated reads of the few files that stayed local. After every final run:
- the churn files on the client matched the server's byte for byte;
- no file carried a version made by the client, and neither side had a conflict copy;
- all 19,816 V8 files read back correctly from cold (55 s).

The remaining failures are reads whose file changed on the server twice within one fetch. The server's own index lags
its disk by its watcher delay (10 s), so there is briefly no version anyone can serve. mountpoint-s3 returns `ESTALE`
in the same situation.

**The zero read was an eviction bug, not a stale-version one.** The kernel decides whether an open file raises
pre-content events in `do_dentry_open()` (`fsnotify_open_perm_and_set_mode`), *before* the open waits for a lease
(`break_lease`). Eviction took the lease first and marked the file afterwards. So an open that raced it was classified
"no watchers", waited for the lease, and then read the fresh placeholder's zeros.
- **The fix** (`hsm.Listener.Evict`): mark the inode before taking the lease. Every access eviction makes itself goes
  through the private mount, which raises no events. Its truncation would otherwise raise an event whose event fd
  opens the file and waits for our own lease.
- **Tests:** `TestEvictRacingOpen` opens the file while the lease is held. With the old order the reader got 12,000
  zero bytes; with the new one, the content.

## 5. Remaining gaps

- **Partial placeholders while tether is stopped.** A crash mid-hydration leaves data blocks in a placeholder until
  the next scan. If tether is stopped entirely, the guard's `i_blocks > 8` fast path lets such a file through, and it
  reads as a mix of real data and zeros. Closing this means the guard reads the xattr for every unmarked file with
  blocks, which costs something on every open system-wide. This is not measured yet.
- **A different mode or ownership in the new version** is left to the puller (the read fails with EIO, as before).
  Switching could apply them too.
- **Superseded by a deletion:** the read fails with EIO until the puller deletes the file. File Provider does the same
  (`noSuchItem`).

## Sources

- Windows `CF_OPERATION_PARAMETERS` (`RESTART_HYDRATION`):
  https://learn.microsoft.com/en-us/windows/win32/api/cfapi/ns-cfapi-cf_operation_parameters
- `CfUpdatePlaceholder`: https://learn.microsoft.com/en-us/windows/win32/api/cfapi/nf-cfapi-cfupdateplaceholder
- CloudMirror sample:
  https://github.com/microsoft/Windows-classic-samples/blob/main/Samples/CloudMirror/CloudMirror/FileCopierWithProgress.cpp
- File Provider `fetchContents`:
  https://developer.apple.com/documentation/fileprovider/nsfileproviderreplicatedextension/fetchcontents(for:version:request:completionhandler:)
- `fetchPartialContents` and `.strictVersioning`:
  https://developer.apple.com/documentation/fileprovider/nsfileproviderpartialcontentfetching/fetchpartialcontents(for:version:request:minimalrange:aligningto:options:completionhandler:)
- mountpoint-s3 semantics: https://github.com/awslabs/mountpoint-s3/blob/main/doc/SEMANTICS.md
- mountpoint-s3 errors: https://github.com/awslabs/mountpoint-s3/blob/main/mountpoint-s3-fs/src/fs/error.rs
- rclone VFS cache: https://github.com/rclone/rclone/blob/master/vfs/vfscache/item.go and
  https://rclone.org/commands/rclone_mount/
- SeaDrive: https://github.com/haiwen/seadrive-fuse/blob/master/src/file-cache-mgr.c
- Nextcloud hydration: https://github.com/nextcloud/desktop/blob/master/src/libsync/vfs/cfapi/hydrationjob.cpp
- EdenFS inodes: https://github.com/facebook/sapling/blob/main/eden/fs/docs/Inodes.md
- ProjFS `PRJ_GET_FILE_DATA_CB`: https://learn.microsoft.com/en-us/windows/win32/api/projectedfslib/nc-projectedfslib-prj_get_file_data_cb
- goofys S3 backend: https://github.com/kahing/goofys/blob/master/internal/backend_s3.go
