#!/usr/bin/env bash
# Phase 0 conformance matrix for fanotify pre-content hydration.
# Needs root and a kernel >= 6.14. Builds a loop-mounted ext4 fs (or the fs
# named by FSTYPE), fills placeholders, and exercises every access path.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
FSTYPE=${FSTYPE:-ext4}
WORK=${WORK:-$(mktemp -d /tmp/hsm-spike.XXXX)}
LOWER=$WORK/lower MNT=$WORK/mnt REMOTE=$WORK/remote REF=$WORK/ref
SPIKE=${SPIKE:-/tmp/hsm-spike} IORING=${IORING:-/tmp/ioring_read}
PASS=0 FAIL=0

cleanup() {
	[ -n "${LPID:-}" ] && kill "$LPID" 2>/dev/null
	umount "$MNT" 2>/dev/null
	umount "$LOWER" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT
[ -n "${KEEPLOG:-}" ] && trap "cp \$WORK/listener.log /tmp/listener.log; cleanup" EXIT

mkdir -p "$LOWER" "$MNT" "$REMOTE" "$REF"
truncate -s 512M "$WORK/fs.img"
mkfs."$FSTYPE" -q "$WORK/fs.img" >/dev/null 2>&1 || mkfs."$FSTYPE" "$WORK/fs.img" >/dev/null
mount -o loop "$WORK/fs.img" "$LOWER"
# Users see $MNT (a bind mount carrying the fanotify mark). Placeholders are
# created through $LOWER, which is unmarked, so their creation never races
# the listener.
mount --bind "$LOWER" "$MNT"

head -c 20000000 /dev/urandom >"$REMOTE/big.bin"
printf 'hello placeholder\n' >"$REMOTE/small.txt"
printf '#!/bin/sh\necho script-ran\n' >"$REMOTE/script.sh"; chmod +x "$REMOTE/script.sh"
cp /bin/true "$REMOTE/truebin"
: >"$REMOTE/empty"

placeholder() { # name -> recreate placeholder + fresh reference copy
	local n=$1
	rm -f "$LOWER/$n"
	truncate -s "$(stat -c %s "$REMOTE/$n")" "$LOWER/$n"
	chmod "$(stat -c %a "$REMOTE/$n")" "$LOWER/$n"
	touch -r "$REMOTE/$n" "$LOWER/$n"
	python3 -c "import os,sys; os.setxattr(sys.argv[1], 'user.tether.state', b'virtual')" "$LOWER/$n"
	cp -p "$REMOTE/$n" "$REF/$n"
}

check() { # desc expected actual
	if [ "$2" = "$3" ]; then echo "PASS  $1"; PASS=$((PASS+1)); else echo "FAIL  $1: want '$2' got '$3'"; FAIL=$((FAIL+1)); fi
}

probe_cmp() { # desc file mode args...
	local desc=$1 f=$2; shift 2
	placeholder "$f"
	local want got
	want=$(python3 "$HERE/probe.py" "$1" "$REF/$f" "${@:2}" 2>&1 | sed "s#$REF#X#")
	got=$(python3 "$HERE/probe.py" "$1" "$MNT/$f" "${@:2}" 2>&1 | sed "s#$MNT#X#")
	check "$desc" "$want" "$got"
}

start_listener() {
	"$SPIKE" -mount "$MNT" -remote "$REMOTE" "$@" >"$WORK/listener.out" 2>>"$WORK/listener.log" &
	LPID=$!
	for _ in $(seq 50); do grep -q READY "$WORK/listener.out" 2>/dev/null && return; sleep 0.1; done
	echo "listener failed to start"; cat "$WORK/listener.log"; exit 1
}

echo "== kernel $(uname -r), fs $FSTYPE"

# -- without a listener placeholders read as zeros (the known hazard) --
placeholder small.txt
check "no listener: placeholder reads zeros" "$(head -c 18 /dev/zero | sha256sum | cut -d' ' -f1)" "$(python3 "$HERE/probe.py" read "$MNT/small.txt")"

start_listener -v -full-below 0   # range hydration only, except for sparse copiers

placeholder big.bin
check "stat size of placeholder" "20000000" "$(stat -c %s "$MNT/big.bin")"
check "placeholder allocates no blocks" "0" "$(stat -c %b "$MNT/big.bin")"

probe_cmp "read()" big.bin read
probe_cmp "read() small" small.txt read
probe_cmp "read() empty file" empty read
probe_cmp "pread mid-range" big.bin pread 10000000 5000
blocks=$(stat -c %b "$MNT/big.bin")
check "partial hydration stays sparse (<1MiB allocated)" "yes" "$([ "$blocks" -lt 2048 ] && echo yes || echo "no:$blocks")"
probe_cmp "mmap read" big.bin mmap-read
probe_cmp "mmap write (shared)" big.bin mmap-write 12345
probe_cmp "pwrite without prior read" big.bin pwrite 777777
probe_cmp "append" small.txt append
probe_cmp "truncate shrink" big.bin truncate 1000
probe_cmp "truncate grow" small.txt truncate 100000
probe_cmp "sendfile" big.bin sendfile
probe_cmp "splice" big.bin splice
probe_cmp "copy_file_range (same fs)" big.bin copy_file_range
probe_cmp "O_DIRECT read" big.bin odirect

placeholder big.bin
check "io_uring read" "$(sha256sum <"$REMOTE/big.bin" | cut -d' ' -f1)" "$("$IORING" "$MNT/big.bin" | sha256sum | cut -d' ' -f1)"
placeholder script.sh
check "exec script" "script-ran" "$("$MNT/script.sh" 2>&1)"
placeholder truebin
"$MNT/truebin"; check "exec ELF binary" "0" "$?"
placeholder big.bin
check "cp" "$(sha256sum <"$REMOTE/big.bin" | cut -d' ' -f1)" "$(cp "$MNT/big.bin" "$MNT/cp.out" && sha256sum <"$MNT/cp.out" | cut -d' ' -f1)"

placeholder big.bin
check "mv across filesystems (sparse-aware copier)" "$(sha256sum <"$REMOTE/big.bin" | cut -d' ' -f1)" "$(mv "$MNT/big.bin" "$WORK/mv.out" && sha256sum <"$WORK/mv.out" | cut -d' ' -f1)"
placeholder big.bin
check "mtime preserved after hydration" "$(stat -c %Y "$REMOTE/big.bin")" "$(cat "$MNT/big.bin" >/dev/null; stat -c %Y "$MNT/big.bin")"
check "state xattr flips to local" "local" "$(python3 -c "import os,sys; print(os.getxattr(sys.argv[1],'user.tether.state').decode())" "$MNT/big.bin")"

# -- errors propagate as errno --
placeholder small.txt; rm "$REMOTE/small.txt"
check "missing remote -> EIO" "ERR 5 Input/output error" "$(python3 "$HERE/probe.py" read "$MNT/small.txt")"
printf 'hello placeholder\n' >"$REMOTE/small.txt"

# -- listener dies while holding an event --
placeholder big.bin
kill -STOP "$LPID"
( python3 "$HERE/probe.py" read "$MNT/big.bin" >"$WORK/blocked.out" ) & RP=$!
sleep 0.5
check "reader blocks while listener is stopped" "running" "$(kill -0 $RP 2>/dev/null && echo running)"
kill -KILL "$LPID"; wait "$LPID" 2>/dev/null; LPID=
wait $RP
check "listener death releases blocked reader with zeros (hazard)" "$(head -c 20000000 /dev/zero | sha256sum | cut -d' ' -f1)" "$(cat "$WORK/blocked.out")"

# -- overhead on already-local files --
start_listener
mkdir -p "$MNT/many"; for i in $(seq 2000); do echo "$i" >"$MNT/many/$i"; done
sync; echo 3 >/proc/sys/vm/drop_caches
t0=$(date +%s%N); cat "$MNT"/many/* >/dev/null; t1=$(date +%s%N)
cat "$MNT"/many/* >/dev/null; t2=$(date +%s%N)
kill "$LPID"; wait "$LPID" 2>/dev/null; LPID=
cat "$MNT"/many/* >/dev/null; t3=$(date +%s%N)
echo "INFO  2000 local files: cold+listener $(( (t1-t0)/1000000 ))ms, warm+ignore-marks $(( (t2-t1)/1000000 ))ms, warm no listener $(( (t3-t2)/1000000 ))ms"

echo "== $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
