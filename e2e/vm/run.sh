#!/usr/bin/env bash
# Boot kernels under QEMU and run the kernel-level on-demand checks on ext4,
# xfs and btrfs. Usage: e2e/vm/run.sh [KERNEL_VERSION...]
# Needs: qemu-system-x86_64, mkfs.xfs, mkfs.btrfs, busybox (static), zstd, gcc, go.
# Kernels come from Ubuntu's archive (apt-get download linux-image-unsigned-V linux-modules-V).
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
W=${VM_WORK:-/tmp/tether-vm}
VERSIONS=("${@:-6.14.0-37-generic 6.17.0-40-generic}")
read -ra VERSIONS <<<"${VERSIONS[*]}"
mkdir -p "$W/debs" "$W/bin"

# Guest binaries (static).
(cd "$REPO/e2e/probe" && CGO_ENABLED=0 go build -o "$W/bin/probe" .)
(cd "$REPO/tether" && CGO_ENABLED=0 go test -c -o "$W/bin/hsm.test" ./lib/hsm)
(cd "$REPO/spikes/fanotify-hsm" && CGO_ENABLED=0 go build -o "$W/bin/spike" . && gcc -O2 -static -o "$W/bin/ioring_read" testdata/ioring_read.c)

for V in "${VERSIONS[@]}"; do
	K=$W/k-$V
	if [ ! -d "$K/boot" ]; then
		(cd "$W/debs" && apt-get download "linux-image-unsigned-$V" "linux-modules-$V")
		mkdir -p "$K"
		for d in "$W"/debs/*"${V%-generic}"*.deb; do dpkg-deb -x "$d" "$K"; done
	fi
	MD=$K/lib/modules/$V
	[ -f "$MD/modules.dep" ] || depmod -b "$K" "$V"
	# initramfs: busybox, the xfs/btrfs modules with their dependencies (in load order), test binaries.
	I=$W/initrd-$V; rm -rf "$I"; mkdir -p "$I"/{bin,proc,sys,dev,tmp,mnt,mods,test}
	cp /usr/bin/busybox "$I/bin/busybox"
	cp "$HERE/init" "$I/init"; chmod +x "$I/init"
	cp "$W"/bin/* "$HERE/guest-tests.sh" "$I/test/"
	python3 - "$MD" "$I" <<'PY'
import os, subprocess, sys
md, out = sys.argv[1], sys.argv[2]
deps = {}
for line in open(os.path.join(md, "modules.dep")):
    mod, _, rest = line.partition(":")
    deps[mod] = rest.split()
order = []
def visit(m):
    if m in order: return
    for d in deps.get(m, []): visit(d)
    order.append(m)
for want in ("kernel/fs/xfs/xfs.ko.zst", "kernel/fs/btrfs/btrfs.ko.zst"):
    visit(want)
names = []
for m in order:
    name = os.path.basename(m).removesuffix(".zst")
    subprocess.run(["zstd", "-q", "-d", "-f", os.path.join(md, m), "-o", os.path.join(out, "mods", name)], check=True)
    names.append(name)
open(os.path.join(out, "modules.order"), "w").write("\n".join(names) + "\n")
PY
	(cd "$I" && find . | cpio -o -H newc --quiet | gzip -1) > "$W/initrd-$V.gz"

	# Fresh disks each boot.
	for fs in ext4 xfs btrfs; do
		rm -f "$W/$fs.img"; truncate -s 3G "$W/$fs.img"
		case $fs in
			ext4) mkfs.ext4 -q -F "$W/$fs.img" ;;
			xfs) mkfs.xfs -q -f "$W/$fs.img" ;;
			btrfs) mkfs.btrfs -q -f "$W/$fs.img" ;;
		esac
	done
	echo "=== booting $V (TCG, this takes a while)"
	timeout 1800 qemu-system-x86_64 -machine q35 -cpu max -smp 4 -m 3G -nographic -no-reboot \
		-kernel "$K/boot/vmlinuz-$V" -initrd "$W/initrd-$V.gz" \
		-append "console=ttyS0 panic=-1 quiet" \
		-drive file="$W/ext4.img",if=virtio,format=raw \
		-drive file="$W/xfs.img",if=virtio,format=raw \
		-drive file="$W/btrfs.img",if=virtio,format=raw \
		| tee "$W/console-$V.log" | grep -E --line-buffered "^(kernel|RESULT|COST|==|ALLDONE)|FAIL|panic" || true
done
