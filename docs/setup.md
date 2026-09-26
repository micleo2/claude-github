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

The client needs `CAP_SYS_ADMIN` (fanotify pre-content groups are privileged, and the view is a mount). Rather than running the
daemon as root, give a root-owned copy of the binary that capability and run it as yourself, so downloaded files are
yours:

```sh
sudo install -m755 -o root -g root syncthing /usr/local/bin/tether-syncthing
sudo setcap cap_sys_admin+ep /usr/local/bin/tether-syncthing
tether-syncthing serve --home ~/.local/state/tether-client --no-browser
```

- **Run it long-term** as a systemd user service with `ExecStart` as above. `loginctl enable-linger` keeps it running
  without a login session.
- **Coexisting with Syncthing:** if a normal Syncthing already runs on this machine, give tether other ports. Set the
  GUI address in the config (*Settings → GUI*, or `PATCH /rest/config/gui`) rather than with `--gui-address`, which is
  not saved, so the `tether` CLI finds the right daemon. Set the sync port in `listenAddresses`.
- **Keep the `serve` command.** It runs a small monitor process that owns the fanotify group and restarts the sync
  process if it crashes; meanwhile accesses to placeholders wait instead of reading zeros. With systemd, stop the
  service rather than killing processes: stopping it closes the group (see *Known rough edges*).
- **Self-upgrade is compiled out.** Syncthing's release feed would replace tether with stock Syncthing.

## 4. Pair client and server

- **On the client:** add the server's device ID, with the address `tcp://server.example:22000`, or `dynamic` if you
  use discovery.
- **On the server:** accept the pending-device prompt, or add the client's ID.
- **Keep it a hub:** don't pair clients with each other. Each client talks only to the server.
- **Optional:** with an explicit server address you can disable global discovery, local discovery and relays on the
  clients (*Settings → Connections*).

## 5. Make the client on-demand, then accept the share

Turn on on-demand in the client's **default folder settings**, once. Every folder the client accepts after that is
on-demand, whether through the GUI's accept button, auto-accept or REST:

```sh
KEY=$(sed -n 's:.*<apikey>\(.*\)</apikey>.*:\1:p' ~/.local/state/tether-client/config.xml)
curl -X PATCH -H "X-API-Key: $KEY" http://127.0.0.1:8384/rest/config/defaults/folder \
  -d '{"onDemand": true, "path": "/home/me"}'
```

Then, on the server, share the folder with the client (edit the folder → *Sharing*) and accept it on the client. For a
folder labelled `Docs`, the default path above gives `/home/me/Docs`:

- **The path you choose** becomes the view, where you work. It must be empty; tether refuses to mount over files.
- **The synced data** goes to `<data dir>/ondemand/<folder ID>` (`~/.local/state/tether-client/ondemand/…` here). Don't
  use it directly.
- **Within seconds** the whole tree appears as online-only files.

To place the data yourself, set both `path` and `onDemandView` when creating the folder. An on-demand folder that
can't run (no view, not send-receive, no kernel support) stops with an error. It never falls back to downloading
everything.

The server needs none of this. Leave its defaults alone, so that a folder it accepts is a full copy.

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
| `cacheBudget` | 0 (unlimited) | Above this size, evict least-recently-used unpinned files down to 80% of it (e.g. `20 GB`, `10 %`) |
| `prefetchMaxFileKiB` | 256 | Sibling prefetch size limit; 0 disables prefetch |
| `hydrationTimeoutS` | 60 | How long an open may wait for an unresponsive peer before failing with `EIO` |
| `hydrationDenyExes` | – | Extra executables that may not trigger downloads (indexers are denied by default) |

`prefetchConcurrency` (default 16) also exists, but changing it restarts the folder.

## Known rough edges

- **While tether is stopped,** the placeholder guard fails opens of online-only files with `EIO`. It needs the bpf LSM
  (`bpf` listed in `/sys/kernel/security/lsm`); the log says "Placeholder guard active" or explains why not. Without
  it, placeholders read as zeros while tether is stopped. The guard stays after the service stops. To remove it (e.g.
  when uninstalling): `rm <data dir>/guard/placeholder-guard && umount <data dir>/guard`. A reboot removes it until
  tether starts. See [research/daemon-restart.md](research/daemon-restart.md).
- **Privileges.** With `setcap`, anyone who can run that binary gets `CAP_SYS_ADMIN` in it. The planned fix is a small
  privileged helper plus an unprivileged per-user daemon.
- **Settings UI.** The GUI has no dedicated on-demand controls. The fields should show up in the folder's *Advanced*
  editor, but that is untested; REST, `config.xml` and the `tether` CLI are the tested routes.
- **Auto-accept** creates the view under the default path and refuses a path that already exists.
