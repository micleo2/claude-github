# Setting up a tether hub

A tether deployment is one **server** (the hub) holding plain files, plus any number of **clients** that see the whole
tree but download file contents on demand. This guide sets up a server and one client. Repeat the client steps for
more.

This is the manual flow; there are no packages, systemd units or setup command yet. The e2e harness automates the
same steps (`Cluster.configure` in [`e2e/tether_e2e.py`](../e2e/tether_e2e.py)), so that is the reference
configuration.

## 0. Requirements

| | Server | Each client |
|---|---|---|
| Kernel | Any Linux | ≥ 6.14 |
| Filesystem | Any | ext4, xfs or btrfs for the folder's real path (the kernel only offers pre-content events there) |
| Privileges | Normal user | `CAP_SYS_ADMIN` (in practice root) |
| Network | Reachable on TCP 22000 | Can reach the server |

## 1. Build

```sh
cd tether
go run build.go build          # builds ./syncthing, GUI assets and version string included
go build ./cmd/tether          # the tether CLI, needed on clients
```

The build uses cgo when a C compiler is available (the C SQLite is faster) and falls back to pure Go otherwise.
There is nothing to configure. Copy `syncthing` to every machine, and `tether` to the clients.

## 2. Server

```sh
syncthing serve --home ~/.local/state/tether --no-browser
```

- **Run it long-term** as a systemd user service; Syncthing's unit files are in `tether/etc/linux-systemd/`.
- **Folder:** in the GUI (`http://127.0.0.1:8384`), add a normal **send-receive** folder, e.g. ID `docs`, path
  `/srv/tether/docs`. Nothing on-demand-specific is needed on the server.
- **Device ID:** note it from *Actions → Show ID*, or `GET /rest/system/status` → `myID`.

## 3. Client daemon

The client needs `CAP_SYS_ADMIN` (fanotify pre-content groups and mount marks are privileged), so for now it runs as
root:

```sh
sudo syncthing serve --home /var/lib/tether --no-browser --gui-address 127.0.0.1:8384
```

Keep the `serve` command. It runs a small monitor process which unmounts the on-demand view if the main process
crashes, so placeholders are never readable as zeros.

## 4. Pair client and server

- **On the client:** add the server's device ID, with the address `tcp://server.example:22000`, or `dynamic` if you
  use discovery.
- **On the server:** accept the pending-device prompt, or add the client's ID.
- **Keep it a hub:** don't pair clients with each other. Each client talks only to the server.
- **Optional:** with an explicit server address you can disable global discovery, local discovery and relays on the
  clients (*Settings → Connections*).

## 5. Create the on-demand folder on the client, then share it

**Order matters.** If the client simply accepts the server's share offer, it creates a *normal* folder and starts
downloading everything. Create the folder on the client first, with the same folder ID:

```sh
KEY=$(sudo sed -n 's:.*<apikey>\(.*\)</apikey>.*:\1:p' /var/lib/tether/config.xml)
curl -X PUT -H "X-API-Key: $KEY" http://127.0.0.1:8384/rest/config/folders/docs -d '{
  "id": "docs", "label": "Docs", "type": "sendreceive",
  "path": "/var/lib/tether/data/docs",
  "devices": [{"deviceID": "<CLIENT-ID>"}, {"deviceID": "<SERVER-ID>"}],
  "onDemand": true,
  "onDemandView": "/home/me/Docs",
  "cacheBudget": {"value": 20, "unit": "GB"}
}'
```

- **`path`** is the real directory the daemon works in. Put it somewhere users don't browse.
- **`onDemandView`** is where you work. tether mounts it while the daemon runs. It must not overlap `path`.

Then, on the server, share `docs` with the client (edit the folder → *Sharing*). The two connect, and within seconds
the whole tree appears under `~/Docs` as online-only files.

## 6. Use it

```sh
tether status ~/Docs                   # online-only / local / pinned, per file
du -sh --apparent-size ~/Docs          # full logical size; nothing downloaded
cat ~/Docs/notes/today.md              # downloads on open (siblings are prefetched)
tether pin ~/Docs/Projects/current     # download now and keep local
tether evict ~/Docs/Videos             # free space; files stay listed and openable
```

Folder settings, all changeable without restarting the folder (REST, `config.xml` or the GUI's advanced editor):

| Setting | Default | Meaning |
|---|---|---|
| `pinPatterns` | – | Paths that are always kept local (ignore-file syntax). `tether pin` adds these. |
| `cacheBudget` | 0 (unlimited) | Evict least-recently-used unpinned files above this size (e.g. `20 GB`, `10 %`) |
| `prefetchMaxFileKiB` | 256 | Sibling prefetch size limit; 0 disables prefetch |
| `hydrationTimeoutS` | 60 | How long an open may wait for an unresponsive peer before failing with `EIO` |
| `hydrationDenyExes` | – | Extra executables that may not trigger downloads (indexers are denied by default) |

`prefetchConcurrency` (default 16) also exists, but changing it restarts the folder.

## Known rough edges

- **File ownership.** The client daemon runs as root, so downloaded files are owned by root, and a normal user may not
  be able to write them through the view. Syncthing's `copyOwnershipFromParent` folder option (with `path` owned by
  the user) should give files the user's ownership. This has not been tested with placeholders. The proper fix is the
  planned split into a small privileged helper and an unprivileged per-user daemon.
- **Settings UI.** The GUI has no dedicated on-demand controls. The fields should show up in the folder's *Advanced*
  editor, but that is untested; REST, `config.xml` and the `tether` CLI are the tested routes.
- **Auto-accept.** Don't enable "auto accept" for shared folders on clients. It creates normal, full-download folders.
