#!/usr/bin/env python3
"""End-to-end tests for tether on-demand folders.

Starts a server and two on-demand clients in privileged Docker containers.
Each node gets its own loop-mounted ext4 filesystem (fanotify pre-content
events need a filesystem that supports them; the container's overlayfs does
not). Tests drive the nodes over REST and exercise files through the
clients' views with `docker exec`.

Needs root, Docker and a kernel >= 6.14. Usage:
    e2e/run.sh            # builds the binary, then runs this
    tether_e2e.py [-k substring] [--keep]
"""

import argparse
import hashlib
import json
import os
import random
import shutil
import subprocess
import sys
import time
import traceback
import urllib.error
import urllib.request

IMAGE = os.environ.get("TETHER_E2E_IMAGE", "debian:trixie-slim")
BIN_DIR = os.environ.get("TETHER_BIN_DIR", "/tmp/tether-bin")
WORK = os.environ.get("TETHER_E2E_WORK", "/tmp/tether-e2e")
NET = "tether-e2e"
APIKEY = "tether-e2e-key"
FOLDER = "docs"
LOWER = "/data/lower/docs"
VIEW = "/view/docs"
MiB = 1 << 20


def run(*cmd, check=True, input=None, timeout=120):
    p = subprocess.run(cmd, capture_output=True, text=True, input=input, timeout=timeout)
    if check and p.returncode != 0:
        raise RuntimeError(f"{' '.join(cmd)} failed ({p.returncode}): {p.stderr.strip()}")
    return p


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def wait_for(cond, timeout=60, interval=0.5, what="condition"):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        try:
            last = cond()
            if last:
                return last
        except Exception as e:  # keep polling through transient errors
            last = e
        time.sleep(interval)
    raise AssertionError(f"timed out waiting for {what} (last: {last!r})")


class Node:
    def __init__(self, name, port, ondemand):
        self.name = name
        self.port = port
        self.ondemand = ondemand
        self.container = f"tether-e2e-{name}"
        self.root = os.path.join(WORK, name)
        self.mnt = os.path.join(self.root, "mnt")
        self.id = None

    # -- host-side paths (the ext4 filesystem is mounted on the host too) --
    def lower(self, rel=""):
        return os.path.join(self.mnt, "lower", FOLDER, rel)

    # -- lifecycle --
    def create(self):
        os.makedirs(self.mnt, exist_ok=True)
        img = os.path.join(self.root, "fs.img")
        run("truncate", "-s", "2G", img)
        run("mkfs.ext4", "-q", "-F", img)
        run("mount", "-o", "loop", img, self.mnt)
        os.makedirs(self.lower(), exist_ok=True)
        os.makedirs(os.path.join(self.mnt, "home"), exist_ok=True)
        self.start()

    def start(self):
        run("docker", "rm", "-f", self.container, check=False)
        run("docker", "run", "-d", "--privileged", "--name", self.container, "--hostname", self.name,
            "--network", NET, "-p", f"127.0.0.1:{self.port}:8384",
            "-v", f"{self.mnt}:/data", "-v", f"{BIN_DIR}:/opt/tether:ro",
            "-e", f"STGUIAPIKEY={APIKEY}", "-e", "STNOUPGRADE=1",
            IMAGE, "/opt/tether/syncthing", "serve", "--home", "/data/home", "--no-browser",
            "--gui-address", "http://0.0.0.0:8384", "--no-port-probing")
        st = wait_for(lambda: self.api("GET", "/rest/system/status"), 60, what=f"{self.name} API")
        self.id = st["myID"]

    def destroy(self):
        run("docker", "rm", "-f", self.container, check=False)
        run("umount", "-l", self.mnt, check=False)

    # -- REST --
    def api(self, method, path, body=None, expect_error=False):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(f"http://127.0.0.1:{self.port}{path}", data=data, method=method,
                                     headers={"X-API-Key": APIKEY, "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=120) as r:
                raw = r.read()
        except urllib.error.HTTPError as e:
            raw = e.read()
            if not expect_error:
                raise RuntimeError(f"{self.name} {method} {path}: HTTP {e.code}: {raw[:300]!r}")
            try:
                return json.loads(raw)
            except ValueError:
                return {"error": raw.decode(errors="replace"), "status": e.code}
        return json.loads(raw) if raw.strip() else None

    def db_file(self, rel):
        return self.api("GET", f"/rest/db/file?folder={FOLDER}&file={urllib.request.quote(rel)}")

    def status(self, prefix=""):
        res = self.api("GET", f"/rest/ondemand/status?folder={FOLDER}&prefix={urllib.request.quote(prefix)}")
        return {f["name"]: f for f in res}

    def od(self, op, path, expect_error=False):
        return self.api("POST", f"/rest/ondemand/{op}?folder={FOLDER}&path={urllib.request.quote(path)}",
                        expect_error=expect_error)

    def connected(self, other):
        conns = self.api("GET", "/rest/system/connections")["connections"]
        return conns.get(other.id, {}).get("connected", False)

    def pause(self, other):
        self.api("POST", f"/rest/system/pause?device={other.id}")
        wait_for(lambda: not self.connected(other), 30, what=f"{self.name} disconnected from {other.name}")

    def resume(self, other):
        self.api("POST", f"/rest/system/resume?device={other.id}")
        wait_for(lambda: self.connected(other), 60, what=f"{self.name} reconnected to {other.name}")

    # -- container-side --
    def sh(self, script, check=True, timeout=120):
        p = run("docker", "exec", self.container, "sh", "-c", script, check=False, timeout=timeout)
        if check and p.returncode != 0:
            raise RuntimeError(f"{self.name}: {script!r} failed ({p.returncode}): {p.stderr.strip()}")
        return p

    def view_sha(self, rel):
        p = self.sh(f"sha256sum '{VIEW}/{rel}'", check=False)
        if p.returncode != 0:
            return "ERR " + p.stderr.strip()
        return p.stdout.split()[0]

    def logs(self, tail=200):
        p = run("docker", "logs", "--tail", str(tail), self.container, check=False)
        return p.stdout + p.stderr

    # -- placeholder inspection on the lower filesystem --
    def is_placeholder(self, rel):
        try:
            return os.getxattr(self.lower(rel), "user.tether.state", follow_symlinks=False) == b"virtual"
        except OSError:
            return False

    def allocated(self, rel):
        return os.lstat(self.lower(rel)).st_blocks * 512


class Cluster:
    def __init__(self):
        self.server = Node("server", 18384, ondemand=False)
        self.c1 = Node("c1", 18385, ondemand=True)
        self.c2 = Node("c2", 18386, ondemand=True)
        self.nodes = [self.server, self.c1, self.c2]

    def up(self):
        self.down()
        run("docker", "network", "create", NET)
        for n in self.nodes:
            n.create()
        for n in self.nodes:
            self.configure(n)

    def down(self):
        for n in [self.server, self.c1, self.c2]:
            n.destroy()
        run("docker", "network", "rm", NET, check=False)
        if os.path.isdir(WORK):
            for n in self.nodes:
                run("umount", "-l", n.mnt, check=False)
            shutil.rmtree(WORK, ignore_errors=True)

    def configure(self, n):
        n.api("PATCH", "/rest/config/options", {
            "globalAnnounceEnabled": False, "localAnnounceEnabled": False, "relaysEnabled": False,
            "natEnabled": False, "urAccepted": -1, "crashReportingEnabled": False,
            "listenAddresses": ["tcp://0.0.0.0:22000"], "reconnectionIntervalS": 2,
        })
        peers = [self.server] if n is not self.server else [self.c1, self.c2]
        for p in peers:
            n.api("PUT", f"/rest/config/devices/{p.id}", {
                "deviceID": p.id, "name": p.name, "addresses": [f"tcp://{p.container}:22000"],
            })
        folder = {
            "id": FOLDER, "label": FOLDER, "path": LOWER, "type": "sendreceive",
            "devices": [{"deviceID": n.id}] + [{"deviceID": p.id} for p in peers],
            "fsWatcherEnabled": True, "fsWatcherDelayS": 1, "rescanIntervalS": 3600,
        }
        if n.ondemand:
            folder.update({"onDemand": True, "onDemandView": VIEW})
        n.api("PUT", f"/rest/config/folders/{FOLDER}", folder)

    # -- helpers --
    def write_server(self, rel, data: bytes, mode=0o644):
        p = self.server.lower(rel)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        with open(p + ".e2etmp", "wb") as f:
            f.write(data)
        os.chmod(p + ".e2etmp", mode)
        os.replace(p + ".e2etmp", p)
        self.server.api("POST", f"/rest/db/scan?folder={FOLDER}&sub={urllib.request.quote(rel)}")

    def synced(self, n, rel, data_sha=None):
        """n's index has the global version of rel."""
        f = n.db_file(rel)
        if f["local"]["version"] != f["global"]["version"] or f["global"].get("deleted"):
            return False
        return True

    def wait_placeholder(self, n, rel, timeout=60):
        wait_for(lambda: self.synced(n, rel) and n.is_placeholder(rel), timeout,
                 what=f"placeholder {rel} on {n.name}")


# ---------------------------------------------------------------- tests --

TESTS = []


def test(fn):
    TESTS.append(fn)
    return fn


FILES = {}


@test
def placeholders_created(c):
    """Server files show up on clients as placeholders with correct metadata."""
    rnd = random.Random(1)
    FILES["small.txt"] = b"hello tether\n"
    FILES["big.bin"] = rnd.randbytes(9 * MiB + 12345)
    FILES["empty"] = b""
    FILES["dir/ü naïve.txt"] = "unicode ✓\n".encode()
    FILES["sparse.bin"] = b"\0" * (4 * MiB) + b"tail" + b"\0" * (3 * MiB)
    FILES["script.sh"] = b"#!/bin/sh\necho script-ran\n"
    for rel, data in FILES.items():
        c.write_server(rel, data, 0o755 if rel.endswith(".sh") else 0o640)
    os.symlink("small.txt", c.server.lower("link"))
    c.server.api("POST", f"/rest/db/scan?folder={FOLDER}")

    for n in (c.c1, c.c2):
        for rel, data in FILES.items():
            if data:
                c.wait_placeholder(n, rel)
            else:
                wait_for(lambda: c.synced(n, rel), what=f"{rel} on {n.name}")
            st = os.lstat(n.lower(rel))
            src = os.lstat(c.server.lower(rel))
            assert st.st_size == len(data), (rel, st.st_size)
            assert int(st.st_mtime) == int(src.st_mtime), (rel, st.st_mtime, src.st_mtime)
            assert st.st_mode & 0o777 == src.st_mode & 0o777, (rel, oct(st.st_mode))
            if data:
                assert n.allocated(rel) <= 4096, (rel, n.allocated(rel))
        wait_for(lambda: os.path.islink(n.lower("link")), what="symlink")
        assert os.readlink(n.lower("link")) == "small.txt"
        # Stat through the view reports the real size without hydrating.
        out = n.sh(f"stat -c %s '{VIEW}/big.bin'").stdout.strip()
        assert out == str(len(FILES["big.bin"])), out
        assert n.is_placeholder("big.bin")
    st = c.c1.status()
    assert st["big.bin"]["state"] == "online-only", st["big.bin"]


@test
def read_hydrates(c):
    """Reading through the view returns the right content and makes the file local."""
    for rel in ("small.txt", "big.bin", "dir/ü naïve.txt", "sparse.bin"):
        assert c.c1.view_sha(rel) == sha(FILES[rel]), rel
        assert not c.c1.is_placeholder(rel), rel
    st = c.c1.status()
    assert st["big.bin"]["state"] == "local", st["big.bin"]
    # mtime survived hydration
    assert int(os.lstat(c.c1.lower("big.bin")).st_mtime) == int(os.lstat(c.server.lower("big.bin")).st_mtime)
    # the zero runs of the sparse file stay holes
    assert c.c1.allocated("sparse.bin") < 1 * MiB, c.c1.allocated("sparse.bin")
    # the index agrees and the server still sees c1 in sync
    f = c.c1.db_file("big.bin")
    assert f["local"]["version"] == f["global"]["version"]


@test
def symlink_through_view(c):
    assert c.c2.sh(f"cat '{VIEW}/link'").stdout == FILES["small.txt"].decode()


@test
def exec_placeholder(c):
    """exec of a placeholder script hydrates it first."""
    out = c.c2.sh(f"'{VIEW}/script.sh'").stdout.strip()
    assert out == "script-ran", out


@test
def sparse_aware_copy(c):
    """cp (SEEK_DATA based) out of the view copies content, not holes."""
    c.c2.sh(f"cp '{VIEW}/big.bin' /tmp/copy.bin")
    got = c.c2.sh("sha256sum /tmp/copy.bin").stdout.split()[0]
    assert got == sha(FILES["big.bin"])
    assert not c.c2.is_placeholder("big.bin")


@test
def concurrent_readers(c):
    """Several processes opening the same placeholder at once all get the content."""
    rel = "concurrent.bin"
    FILES[rel] = random.Random(2).randbytes(6 * MiB)
    c.write_server(rel, FILES[rel])
    c.wait_placeholder(c.c1, rel)
    script = " & ".join(f"sha256sum '{VIEW}/{rel}' > /tmp/cr{i}" for i in range(6)) + " & wait; cat /tmp/cr*"
    lines = c.c1.sh(script).stdout.split("\n")
    sums = {l.split()[0] for l in lines if l.strip()}
    assert sums == {sha(FILES[rel])}, sums


@test
def remote_update_of_local_file(c):
    """A remote change to a hydrated (unpinned) file turns it back into a placeholder of the new version."""
    rel = "small.txt"
    assert not c.c1.is_placeholder(rel)
    FILES[rel] = b"hello tether, version 2\n"
    c.write_server(rel, FILES[rel])
    wait_for(lambda: c.c1.db_file(rel)["local"]["size"] == len(FILES[rel]) and c.synced(c.c1, rel),
             what="c1 has v2 metadata")
    assert c.c1.is_placeholder(rel)
    assert c.c1.view_sha(rel) == sha(FILES[rel])


@test
def pin_hydrates_and_stays_local(c):
    """Pinning a directory hydrates it; remote updates to pinned files are downloaded eagerly."""
    rel = "pinned/a.bin"
    FILES[rel] = random.Random(3).randbytes(2 * MiB)
    c.write_server(rel, FILES[rel])
    c.wait_placeholder(c.c2, rel)
    c.c2.od("pin", "pinned")
    assert not c.c2.is_placeholder(rel)
    with open(c.c2.lower(rel), "rb") as f:
        assert sha(f.read()) == sha(FILES[rel])
    st = c.c2.status("pinned")
    assert st[rel]["state"] == "pinned", st
    FILES[rel] = random.Random(4).randbytes(2 * MiB)
    c.write_server(rel, FILES[rel])

    def updated():
        if not c.synced(c.c2, rel) or c.c2.is_placeholder(rel):
            return False
        with open(c.c2.lower(rel), "rb") as f:
            return sha(f.read()) == sha(FILES[rel])
    wait_for(updated, what="pinned file updated in place")
    # pinned files are not evictable
    res = c.c2.od("evict", "pinned")
    assert res["files"] == 0, res
    assert not c.c2.is_placeholder(rel)
    c.c2.od("unpin", "pinned")


@test
def local_edit_of_placeholder(c):
    """Appending to a placeholder through the view hydrates it first and syncs the result."""
    rel = "edit.txt"
    FILES[rel] = b"line one\n"
    c.write_server(rel, FILES[rel])
    c.wait_placeholder(c.c1, rel)
    c.c1.sh(f"echo 'line two' >> '{VIEW}/{rel}'")
    FILES[rel] += b"line two\n"
    assert c.c1.view_sha(rel) == sha(FILES[rel])

    def server_has():
        with open(c.server.lower(rel), "rb") as f:
            return f.read() == FILES[rel]
    wait_for(server_has, what="server received the edit")


@test
def rename_placeholder(c):
    """mv of a placeholder is a rename in the cluster, with no data transfer and no loss."""
    rel, new = "rename-me.bin", "renamed/moved.bin"
    FILES[rel] = random.Random(5).randbytes(3 * MiB)
    c.write_server(rel, FILES[rel])
    c.wait_placeholder(c.c1, rel)
    c.c1.sh(f"mkdir -p '{VIEW}/renamed' && mv '{VIEW}/{rel}' '{VIEW}/{new}'")
    assert c.c1.is_placeholder(new), "rename must not hydrate"

    def server_renamed():
        if os.path.exists(c.server.lower(rel)) or not os.path.exists(c.server.lower(new)):
            return False
        with open(c.server.lower(new), "rb") as f:
            return sha(f.read()) == sha(FILES[rel])
    wait_for(server_renamed, what="server applied the rename with intact content")
    c.wait_placeholder(c.c2, new)
    assert not os.path.exists(c.c2.lower(rel))
    assert c.c2.view_sha(new) == sha(FILES[rel])
    assert c.c1.is_placeholder(new)
    assert c.c1.view_sha(new) == sha(FILES[rel])
    FILES[new] = FILES.pop(rel)


@test
def chmod_placeholder(c):
    """Metadata changes on a placeholder propagate without hydrating."""
    rel = "chmod.bin"
    FILES[rel] = random.Random(6).randbytes(1 * MiB)
    c.write_server(rel, FILES[rel], 0o644)
    c.wait_placeholder(c.c1, rel)
    c.c1.sh(f"chmod 600 '{VIEW}/{rel}'")
    wait_for(lambda: os.lstat(c.server.lower(rel)).st_mode & 0o777 == 0o600, what="mode on server")
    assert c.c1.is_placeholder(rel)
    with open(c.server.lower(rel), "rb") as f:
        assert sha(f.read()) == sha(FILES[rel])


@test
def delete_placeholder(c):
    rel = "delete-me.txt"
    c.write_server(rel, b"bye\n")
    c.wait_placeholder(c.c1, rel)
    c.c1.sh(f"rm '{VIEW}/{rel}'")
    wait_for(lambda: not os.path.exists(c.server.lower(rel)), what="server deleted file")
    wait_for(lambda: not os.path.exists(c.c2.lower(rel)), what="c2 deleted file")


@test
def remote_delete_of_local_file(c):
    rel = "dir/ü naïve.txt"
    assert not c.c1.is_placeholder(rel)
    os.remove(c.server.lower(rel))
    c.server.api("POST", f"/rest/db/scan?folder={FOLDER}")
    wait_for(lambda: not os.path.exists(c.c1.lower(rel)), what="c1 deleted hydrated file")
    FILES.pop(rel)


@test
def evict_and_rehydrate(c):
    rel = "big.bin"
    if c.c1.is_placeholder(rel):
        c.c1.view_sha(rel)
    res = c.c1.od("evict", rel)
    assert res["files"] == 1, res
    assert c.c1.is_placeholder(rel)
    assert c.c1.allocated(rel) <= 4096
    assert int(os.lstat(c.c1.lower(rel)).st_mtime) == int(os.lstat(c.server.lower(rel)).st_mtime)
    assert c.c1.status()[rel]["state"] == "online-only"
    time.sleep(3)  # give the scanner a chance to (wrongly) react
    f = c.c1.db_file(rel)
    assert f["local"]["version"] == f["global"]["version"], "eviction must not create a new version"
    assert c.c1.view_sha(rel) == sha(FILES[rel])


@test
def evict_refused_while_open(c):
    rel = "big.bin"
    c.c1.view_sha(rel)
    c.c1.sh(f"sh -c 'exec 3<\"{VIEW}/{rel}\"; sleep 8' >/dev/null 2>&1 &")
    time.sleep(1)
    res = c.c1.od("evict", rel, expect_error=True)
    assert res["files"] == 0 and "in use" in res.get("error", ""), res
    assert not c.c1.is_placeholder(rel)


@test
def evict_refused_for_unsynced_change(c):
    """A local change the server has not received yet must never be evicted."""
    rel = "unsynced.txt"
    c.c1.pause(c.server)
    try:
        c.c1.sh(f"echo precious > '{VIEW}/{rel}'")
        wait_for(lambda: c.c1.db_file(rel)["local"]["size"] > 0, what="c1 scanned new file")
        res = c.c1.od("evict", rel, expect_error=True)
        assert res["files"] == 0 and "no other device" in res.get("error", ""), res
        assert not c.c1.is_placeholder(rel)
    finally:
        c.c1.resume(c.server)
    wait_for(lambda: os.path.exists(c.server.lower(rel)), 90, what="server got the change after reconnect")
    with open(c.server.lower(rel), "rb") as f:
        assert f.read() == b"precious\n"


@test
def offline_placeholder_read_fails_cleanly(c):
    """Offline, reading a placeholder fails with an error instead of returning zeros; local files still work."""
    rel = "offline.bin"
    FILES[rel] = random.Random(7).randbytes(1 * MiB)
    c.write_server(rel, FILES[rel])
    c.wait_placeholder(c.c1, rel)
    c.wait_placeholder(c.c2, rel)
    local = "big.bin"
    c.c1.view_sha(local)
    c.c1.pause(c.server)
    try:
        t0 = time.time()
        got = c.c1.view_sha(rel)
        assert time.time() - t0 < 5, "offline reads must fail fast"
        assert got.startswith("ERR"), got
        assert c.c1.is_placeholder(rel), "failed read must leave the placeholder"
        assert c.c1.view_sha(local) == sha(FILES[local]), "local files stay readable offline"
    finally:
        c.c1.resume(c.server)
    assert c.c1.view_sha(rel) == sha(FILES[rel])


@test
def peer_placeholder_is_not_a_source(c):
    """With the server gone, c2 can hydrate from c1's local copy, but never from c1's placeholder."""
    have, placeholder = "hydrated-on-c1.bin", "placeholder-on-c1.bin"
    FILES[have] = random.Random(8).randbytes(2 * MiB)
    FILES[placeholder] = random.Random(9).randbytes(2 * MiB)
    c.write_server(have, FILES[have])
    c.write_server(placeholder, FILES[placeholder])
    for n in (c.c1, c.c2):
        c.wait_placeholder(n, have)
        c.wait_placeholder(n, placeholder)
    assert c.c1.view_sha(have) == sha(FILES[have]), "c1 hydrate"
    # c1 and c2 are not configured as peers of each other in the hub layout;
    # share the folder between them for this test.
    for a, b in ((c.c1, c.c2), (c.c2, c.c1)):
        a.api("PUT", f"/rest/config/devices/{b.id}", {"deviceID": b.id, "name": b.name,
                                                       "addresses": [f"tcp://{b.container}:22000"]})
        cfg = a.api("GET", f"/rest/config/folders/{FOLDER}")
        cfg["devices"].append({"deviceID": b.id})
        a.api("PUT", f"/rest/config/folders/{FOLDER}", cfg)
    wait_for(lambda: c.c2.connected(c.c1), 60, what="c1-c2 connected")
    time.sleep(3)  # index exchange
    c.c2.pause(c.server)
    try:
        got = c.c2.view_sha(have)
        assert got == sha(FILES[have]), f"c2 from c1's copy: {got}"
        got = c.c2.view_sha(placeholder)
        assert got.startswith("ERR"), got  # an error, never zeros
    finally:
        c.c2.resume(c.server)
    assert c.c2.view_sha(placeholder) == sha(FILES[placeholder])


@test
def indexer_denied(c):
    rel = "deny.txt"
    c.write_server(rel, b"do not index me\n")
    c.wait_placeholder(c.c1, rel)
    p = c.c1.sh(f"cp /bin/cat /tmp/updatedb && /tmp/updatedb '{VIEW}/{rel}'", check=False)
    assert p.returncode != 0 and "not permitted" in p.stderr, (p.returncode, p.stderr)
    assert c.c1.is_placeholder(rel)
    assert c.c1.sh(f"cat '{VIEW}/{rel}'").stdout == "do not index me\n"


@test
def mmap_odirect_pread(c):
    """mmap, O_DIRECT and a mid-file pread on placeholders all see the real content."""
    rnd = random.Random(20)
    for mode in ("mmap", "odirect", "pread"):
        rel = f"access/{mode}.bin"
        FILES[rel] = rnd.randbytes(3 * MiB + 17)
        c.write_server(rel, FILES[rel])
        c.wait_placeholder(c.c2, rel)
        if mode == "pread":
            out = c.c2.sh(f"/opt/tether/probe pread '{VIEW}/{rel}' 2000000 5000").stdout.strip()
            assert out == sha(FILES[rel][2000000:2005000]), mode
        else:
            out = c.c2.sh(f"/opt/tether/probe {mode} '{VIEW}/{rel}'").stdout.strip()
            assert out == sha(FILES[rel]), mode
        assert not c.c2.is_placeholder(rel), mode


@test
def truncating_overwrite(c):
    """`echo new > placeholder` (O_TRUNC) ends with exactly the new content everywhere."""
    rel = "overwrite.txt"
    c.write_server(rel, b"old content that is longer\n")
    c.wait_placeholder(c.c1, rel)
    c.c1.sh(f"echo new > '{VIEW}/{rel}'")
    FILES[rel] = b"new\n"
    wait_for(lambda: open(c.server.lower(rel), "rb").read() == FILES[rel], what="server has new content")
    wait_for(lambda: c.c2.view_sha(rel) == sha(FILES[rel]), what="c2 sees new content")


@test
def atomic_save_over_placeholder(c):
    """Editors write a temp file and rename it over the original; the result syncs without conflicts."""
    rel = "atomic.txt"
    c.write_server(rel, b"draft 1\n")
    c.wait_placeholder(c.c1, rel)
    c.c1.sh(f"printf 'draft 2\\n' > '{VIEW}/.atomic.tmp' && mv '{VIEW}/.atomic.tmp' '{VIEW}/{rel}'")
    FILES[rel] = b"draft 2\n"
    wait_for(lambda: open(c.server.lower(rel), "rb").read() == FILES[rel], what="server has draft 2")
    assert not [n for n in os.listdir(c.server.lower()) if "sync-conflict" in n and "atomic" in n]


@test
def directory_rename_with_placeholders(c):
    """Renaming a directory full of placeholders moves them all, with no data transfer and no loss."""
    rnd = random.Random(21)
    names = [f"proj/src/f{i}.bin" for i in range(20)]
    for rel in names:
        FILES[rel] = rnd.randbytes(64 * 1024 + i if (i := rnd.randrange(1000)) else 1)
        c.write_server(rel, FILES[rel])
    for rel in names:
        c.wait_placeholder(c.c1, rel)
    c.c1.sh(f"mv '{VIEW}/proj' '{VIEW}/project'")
    moved = [r.replace("proj/", "project/", 1) for r in names]
    for rel in moved:
        assert c.c1.is_placeholder(rel), rel

    def server_moved():
        return not os.path.exists(c.server.lower("proj")) and all(
            os.path.exists(c.server.lower(r)) for r in moved)
    wait_for(server_moved, 90, what="server moved the directory")
    for old, rel in zip(names, moved):
        with open(c.server.lower(rel), "rb") as f:
            assert sha(f.read()) == sha(FILES[old]), rel
        FILES[rel] = FILES.pop(old)
    assert c.c1.view_sha(moved[3]) == sha(FILES[moved[3]])


@test
def listing_does_not_hydrate(c):
    """ls -lR / find / stat over many placeholders never downloads content."""
    rnd = random.Random(22)
    root = c.server.lower("many")
    for d in range(30):
        os.makedirs(os.path.join(root, f"d{d:02}"), exist_ok=True)
        for i in range(50):
            with open(os.path.join(root, f"d{d:02}", f"f{i:02}.txt"), "wb") as f:
                f.write(rnd.randbytes(100 + i))
    t0 = time.time()
    c.server.api("POST", f"/rest/db/scan?folder={FOLDER}&sub=many")
    wait_for(lambda: c.c2.api("GET", f"/rest/db/status?folder={FOLDER}")["needFiles"] == 0 and
             os.path.exists(c.c2.lower("many/d29/f49.txt")), 120, what="1500 placeholders on c2")
    print(f"      1500 placeholders on c2 after {time.time() - t0:.1f}s")
    out = c.c2.sh(f"ls -lR '{VIEW}/many' | grep -c '\\.txt$'; find '{VIEW}/many' -type f | wc -l; "
                  f"du -s --apparent-size '{VIEW}/many'").stdout.split()
    assert out[0] == "1500" and out[1] == "1500", out
    hydrated = [f for f, st in c.c2.status("many").items() if st["state"] != "online-only"]
    assert not hydrated, hydrated[:5]
    # grep -r reads everything and must see real content
    p = c.c2.sh(f"grep -rl --binary-files=text . '{VIEW}/many' | wc -l")
    assert p.stdout.strip() == "1500", p.stdout


@test
def large_file(c):
    rel = "large.bin"
    FILES[rel] = os.urandom(256 * MiB)
    c.write_server(rel, FILES[rel])
    c.wait_placeholder(c.c1, rel, timeout=120)
    t0 = time.time()
    assert c.c1.view_sha(rel) == sha(FILES[rel])
    dt = time.time() - t0
    print(f"      256 MiB hydrated in {dt:.1f}s ({256 / dt:.0f} MiB/s)")


@test
def client_restart_keeps_state(c):
    n = c.c2
    before = n.status()
    run("docker", "restart", n.container)
    wait_for(lambda: n.api("GET", "/rest/system/status"), 60, what="restart")
    wait_for(lambda: n.sh(f"grep -q ' {VIEW} ' /proc/self/mountinfo", check=False).returncode == 0, 30,
             what="view remounted")
    after = n.status()
    assert {k: v["state"] for k, v in before.items()} == {k: v["state"] for k, v in after.items()}
    rel = next(k for k, v in after.items() if v["state"] == "online-only" and k in FILES and FILES[k])
    assert n.view_sha(rel) == sha(FILES[rel])


@test
def offline_conflicting_edits(c):
    """Both sides edit the same (hydrated) file while apart: both versions survive."""
    rel = "conflict.txt"
    c.write_server(rel, b"base\n")
    c.wait_placeholder(c.c1, rel)
    assert c.c1.view_sha(rel) == sha(b"base\n")
    c.c1.pause(c.server)
    try:
        c.c1.sh(f"echo client-edit > '{VIEW}/{rel}'")
        time.sleep(1.5)
        with open(c.server.lower(rel), "wb") as f:
            f.write(b"server-edit\n")
        c.server.api("POST", f"/rest/db/scan?folder={FOLDER}&sub={rel}")
        wait_for(lambda: c.c1.db_file(rel)["local"]["size"] == len(b"client-edit\n"), what="c1 scanned edit")
    finally:
        c.c1.resume(c.server)

    def both_survive():
        contents = set()
        for name in os.listdir(c.server.lower()):
            if name == rel or (name.startswith("conflict.sync-conflict-") and name.endswith(".txt")):
                with open(c.server.lower(name), "rb") as f:
                    contents.add(f.read())
        return {b"client-edit\n", b"server-edit\n"} <= contents
    wait_for(both_survive, 90, what="both versions on the server")


@test
def placeholder_metadata_vs_remote_content_conflict(c):
    """c1 chmods a placeholder while offline, the server rewrites the content: the cluster converges, nothing is lost."""
    rel = "meta-conflict.bin"
    orig = random.Random(24).randbytes(1 * MiB)
    c.write_server(rel, orig)
    c.wait_placeholder(c.c1, rel)
    c.c1.pause(c.server)
    try:
        c.c1.sh(f"chmod 600 '{VIEW}/{rel}'")
        wait_for(lambda: c.c1.db_file(rel)["local"]["permissions"] == "0600", what="c1 scanned chmod")
        assert c.c1.is_placeholder(rel)
        time.sleep(1.5)
        FILES[rel] = random.Random(25).randbytes(1 * MiB)
        c.write_server(rel, FILES[rel])
    finally:
        c.c1.resume(c.server)

    def converged():
        s = c.server.api("GET", f"/rest/db/status?folder={FOLDER}")
        k = c.c1.api("GET", f"/rest/db/status?folder={FOLDER}")
        return (s["needFiles"] == 0 and k["needFiles"] == 0 and c.synced(c.c1, rel) and c.synced(c.server, rel)
                and c.c1.db_file(rel)["global"]["version"] == c.server.db_file(rel)["global"]["version"])
    wait_for(converged, 90, what="cluster converged")
    # The server's new content must exist somewhere on the server, and the
    # file c1 sees must be readable and match what the server holds.
    server_versions = set()
    for name in os.listdir(c.server.lower()):
        if name.startswith("meta-conflict"):
            with open(c.server.lower(name), "rb") as f:
                server_versions.add(sha(f.read()))
    assert sha(FILES[rel]) in server_versions, "server's new content was lost"
    with open(c.server.lower(rel), "rb") as f:
        FILES[rel] = f.read()
    assert c.c1.view_sha(rel) == sha(FILES[rel])



@test
def cache_budget_evicts_lru(c):
    n = c.c2
    names = [f"budget/{i}.bin" for i in range(3)]
    for i, rel in enumerate(names):
        FILES[rel] = random.Random(100 + i).randbytes(5 * MiB)
        c.write_server(rel, FILES[rel])
    for rel in names:
        c.wait_placeholder(n, rel)
    # Evict everything else so the budget math is about these files.
    n.od("evict", "")
    cfg = n.api("GET", f"/rest/config/folders/{FOLDER}")
    cfg["cacheBudget"] = {"value": 12, "unit": "MB"}
    n.api("PUT", f"/rest/config/folders/{FOLDER}", cfg)
    for rel in names:
        assert n.view_sha(rel) == sha(FILES[rel])
        time.sleep(1.1)  # distinct atimes
    wait_for(lambda: n.is_placeholder(names[0]), 30, what="oldest file evicted")
    assert not n.is_placeholder(names[1]) and not n.is_placeholder(names[2])
    cfg["cacheBudget"] = {"value": 0, "unit": ""}
    n.api("PUT", f"/rest/config/folders/{FOLDER}", cfg)


@test
def silent_network_loss_times_out(c):
    """If the server vanishes without closing the connection, a blocked reader gets an error after the timeout."""
    rel = "silent.bin"
    FILES[rel] = random.Random(11).randbytes(1 * MiB)
    c.write_server(rel, FILES[rel])
    c.wait_placeholder(c.c1, rel)
    cfg = c.c1.api("GET", f"/rest/config/folders/{FOLDER}")
    cfg["hydrationTimeoutS"] = 5
    c.c1.api("PUT", f"/rest/config/folders/{FOLDER}", cfg)
    run("docker", "network", "disconnect", NET, c.server.container)
    try:
        t0 = time.time()
        got = c.c1.view_sha(rel)
        took = time.time() - t0
        assert got.startswith("ERR"), got
        assert took < 20, took
        assert c.c1.is_placeholder(rel)
    finally:
        run("docker", "network", "connect", NET, c.server.container)
        cfg["hydrationTimeoutS"] = 60
        c.c1.api("PUT", f"/rest/config/folders/{FOLDER}", cfg)
    wait_for(lambda: c.c1.view_sha(rel) == sha(FILES[rel]), 180, interval=3, what="recovery after reconnect")


@test
def crash_unmounts_view_and_recovers(c):
    """kill -9 of the sync process: the monitor removes the unmarked view; after restart all works."""
    n = c.c1
    rel = "after-crash.bin"
    FILES[rel] = random.Random(10).randbytes(1 * MiB)
    c.write_server(rel, FILES[rel])
    c.wait_placeholder(n, rel)
    procs = run("docker", "top", n.container, "-o", "pid,ppid,args").stdout.splitlines()[1:]
    rows = [p.split(None, 2) for p in procs]
    pids = {r[0] for r in rows if "syncthing" in r[2]}
    child = [r[0] for r in rows if "syncthing" in r[2] and r[1] in pids]
    assert child, procs
    run("kill", "-9", child[0])
    wait_for(lambda: "Unmounted on-demand view left behind" in n.logs(400), 30, what="monitor cleanup")
    wait_for(lambda: n.api("GET", "/rest/system/status"), 60, what="restart")
    wait_for(lambda: n.sh(f"grep -q ' {VIEW} ' /proc/self/mountinfo", check=False).returncode == 0, 30,
             what="view remounted")
    assert n.view_sha(rel) == sha(FILES[rel])


@test
def no_zero_uploads(c):
    """Global invariant: every file the server has matches what was written; nothing became zeros."""
    for rel, data in FILES.items():
        path = c.server.lower(rel)
        if not os.path.exists(path):
            continue
        with open(path, "rb") as f:
            got = f.read()
        if rel in ("edit.txt",):
            continue
        assert sha(got) == sha(data), rel


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-k", help="only run tests whose name contains this")
    ap.add_argument("--keep", action="store_true", help="leave the cluster running afterwards")
    args = ap.parse_args()

    c = Cluster()
    c.up()
    passed, failed = [], []
    try:
        for t in TESTS:
            if args.k and not any(k in t.__name__ for k in args.k.split(",")):
                continue
            start = time.time()
            try:
                t(c)
                passed.append(t.__name__)
                print(f"PASS  {t.__name__} ({time.time() - start:.1f}s)", flush=True)
            except Exception:
                failed.append(t.__name__)
                print(f"FAIL  {t.__name__} ({time.time() - start:.1f}s)", flush=True)
                traceback.print_exc()
    finally:
        if failed:
            for n in c.nodes:
                with open(os.path.join("/tmp", f"tether-e2e-{n.name}.log"), "w") as f:
                    f.write(n.logs(2000))
            print("node logs saved to /tmp/tether-e2e-*.log")
        if not args.keep:
            c.down()
    print(f"== {len(passed)} passed, {len(failed)} failed")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
