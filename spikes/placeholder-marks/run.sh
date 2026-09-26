#!/usr/bin/env bash
# Placeholder-mark spikes (docs/research/placeholder-access-paths.md).
# Needs root and a kernel >= 6.14; builds loop-mounted filesystems.
#   sudo ./run.sh coverage        inode and filesystem marks vs. namespaces
#   sudo ./run.sh markbench       startup cost of per-placeholder inode marks
# Without sudo, the same works in a privileged container, e.g.
#   docker run --rm --privileged -v "$PWD:/s" -w /s alpine:3 sh -c \
#     'apk add -q bash util-linux e2fsprogs btrfs-progs && ./run.sh coverage'
# (build first with: CGO_ENABLED=0 go build -o bin/ ./coverage ./markbench)
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d /tmp/placeholder-marks.XXXXXX)
MNT=$WORK/mnt LOOP=
cleanup() {
	umount "$MNT" 2>/dev/null || true
	[ -n "$LOOP" ] && losetup -d "$LOOP" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT
mkdir -p "$MNT"

bin() { # name -> path of a built binary
	if [ -f "$HERE/bin/$1" ]; then echo "$HERE/bin/$1"; else (cd "$HERE" && go build -o "$WORK/$1" "./$1") && echo "$WORK/$1"; fi
}

fs() { # fstype size [inodes] -> fresh filesystem mounted at $MNT
	umount "$MNT" 2>/dev/null || true
	[ -n "$LOOP" ] && losetup -d "$LOOP"
	rm -f "$WORK/fs.img"
	truncate -s "$2" "$WORK/fs.img"
	case $1 in
	ext4) mkfs.ext4 -q -F ${3:+-N "$3"} "$WORK/fs.img" ;;
	*) "mkfs.$1" -q -f "$WORK/fs.img" ;;
	esac
	LOOP=$(losetup -f --show "$WORK/fs.img")
	mount -t "$1" "$LOOP" "$MNT"
}

case ${1:-} in
coverage)
	b=$(bin coverage)
	for mode in inode sb; do
		fs ext4 256M
		MODE=$mode "$b" "$MNT"
		echo
	done
	;;
markbench)
	b=$(bin markbench)
	for t in ext4 btrfs; do
		for n in 20000 200000 1000000; do
			fs "$t" 16G 1200000
			echo "== $t, $n files"
			"$b" "$MNT" "$n"
		done
	done
	;;
*)
	echo "usage: $0 coverage|markbench" >&2
	exit 2
	;;
esac
