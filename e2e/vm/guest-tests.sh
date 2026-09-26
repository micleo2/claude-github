#!/bin/sh
# Runs inside the VM (busybox). For every mounted test filesystem, checks that
# fanotify pre-content hydration works through each access path, runs the
# lib/hsm tests there, and measures what a placeholder costs on disk.
T=/test
res() { echo "RESULT $FS $1 $2"; }
check() { if [ "$2" = "$3" ]; then res "$1" PASS; else res "$1" "FAIL(want=$2 got=$3)"; fi; }
h() { sha256sum | cut -d' ' -f1; }

for FS in $FSLIST; do
	M=/mnt/$FS
	echo "== $FS: $(grep " $M " /proc/mounts)"
	L=$M/lower V=$M/view R=$M/remote
	mkdir -p $L $V $R $M/tmp
	mount --bind $L $V

	dd if=/dev/urandom of=$R/big.bin bs=1M count=5 2>/dev/null
	printf 'hello placeholder\n' > $R/small.txt
	printf '#!/bin/sh\necho script-ran\n' > $R/script.sh; chmod +x $R/script.sh
	cp $T/probe $R/elf; chmod +x $R/elf
	ph() { $T/probe mkph $L/$1 $(stat -c %s $R/$1) && chmod $(stat -c %a $R/$1) $L/$1; }
	want() { h < $R/$1; }

	ph small.txt
	check no-listener-reads-zeros "$(head -c 18 /dev/zero | h)" "$(h < $V/small.txt)"

	$T/spike -v -mount $V -remote $R -full-below 0 >$M/spike.out 2>$M/spike.log &
	SP=$!
	i=0; while ! grep -q READY $M/spike.out 2>/dev/null && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
	if ! grep -q READY $M/spike.out; then res listener-start "FAIL($(tail -1 $M/spike.log))"; kill $SP 2>/dev/null; umount $V; continue; fi
	res listener-start PASS

	ph big.bin; check placeholder-size 5242880 "$(stat -c %s $L/big.bin)"
	check read "$(want big.bin)" "$(h < $V/big.bin)"
	ph big.bin; check pread-mid "$(dd if=$R/big.bin bs=1 skip=3000000 count=5000 2>/dev/null | h)" "$($T/probe pread $V/big.bin 3000000 5000)"
	check partial-stays-sparse yes "$([ $(stat -c %b $L/big.bin) -lt 2048 ] && echo yes || echo no:$(stat -c %b $L/big.bin))"
	ph big.bin; check mmap "$(want big.bin)" "$($T/probe mmap $V/big.bin)"
	ph big.bin; check odirect "$(want big.bin)" "$($T/probe odirect $V/big.bin)"
	ph big.bin; check io_uring "$(want big.bin)" "$($T/ioring_read $V/big.bin | h)"
	ph big.bin; cp $V/big.bin $M/tmp/copy; check cp "$(want big.bin)" "$(h < $M/tmp/copy)"
	ph small.txt; check read-small "$(want small.txt)" "$(h < $V/small.txt)"
	ph script.sh; check exec-script script-ran "$($V/script.sh 2>&1)"
	ph elf; check exec-elf 2 "$($V/elf >/dev/null 2>&1; echo $?)"
	ph big.bin; n0=$(wc -l < $M/spike.log); truncate -s 1000 $V/big.bin
	echo "EVENTS $FS ftruncate(1000): $(tail -n +$((n0 + 1)) $M/spike.log | grep -o 'mask=[^ ]*.*count=[0-9]*' | sed 's/pid=[^ ]* path=[^ ]* //' | tr '\n' ';')"
	check truncate "$(head -c 1000 $R/big.bin | h)" "$(h < $V/big.bin)"
	ph small.txt; mv $R/small.txt $R/small.gone
	check missing-remote-eio 1 "$(cat $V/small.txt >/dev/null 2>&1; echo $?)"
	mv $R/small.gone $R/small.txt

	kill $SP; wait $SP 2>/dev/null

	# Product mode: whole-file hydration when the file is opened (what tether does).
	$T/spike -v -mount $V -remote $R >$M/spike2.out 2>$M/spike2.log &
	SP=$!
	i=0; while ! grep -q READY $M/spike2.out 2>/dev/null && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
	ph elf; n0=$(wc -l < $M/spike2.log)
	check full-exec-elf 2 "$($V/elf >/dev/null 2>&1; echo $?)"
	echo "EVENTS $FS exec: $(tail -n +$((n0 + 1)) $M/spike2.log | grep -o 'mask=[^ ]*.*count=[0-9]*' | sed 's/pid=[^ ]* path=[^ ]* //' | tr '\n' ';')"
	ph script.sh; check full-exec-script script-ran "$($V/script.sh 2>&1)"
	ph big.bin; check full-mmap "$(want big.bin)" "$($T/probe mmap $V/big.bin)"
	ph big.bin; truncate -s 1000 $V/big.bin; check full-truncate "$(head -c 1000 $R/big.bin | h)" "$(h < $V/big.bin)"
	kill $SP; wait $SP 2>/dev/null
	umount $V

	# lib/hsm tests (lease eviction, concurrent openers, errno, policy, view lifecycle)
	out=$(cd $M/tmp && TMPDIR=$M/tmp $T/hsm.test -test.count=1 2>&1)
	if echo "$out" | grep -q '^PASS$'; then res hsm-unit-tests PASS; else res hsm-unit-tests FAIL; echo "$out" | grep -E -- '--- FAIL|hsm_linux_test' | head; fi

	# Disk cost of 2000 placeholders (1 MiB each) carrying the real xattrs.
	mkdir -p $M/cost; sync
	before=$(df -k $M | tail -1 | awk '{print $3}')
	i=0; while [ $i -lt 2000 ]; do $T/probe mkph $M/cost/p$i 1048576; i=$((i+1)); done
	sync; sleep 1; sync
	after=$(df -k $M | tail -1 | awk '{print $3}')
	echo "COST $FS $(( (after - before) * 1024 / 2000 )) bytes/placeholder (st_blocks of one: $(stat -c %b $M/cost/p0) x512)"
done
echo ALLDONE
