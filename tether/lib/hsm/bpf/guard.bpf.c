// SPDX-License-Identifier: GPL-2.0
//
// tether placeholder guard (a BPF LSM program, see guard_linux.go). While
// tether runs, every placeholder's inode carries its fanotify mark and opens
// wait for hydration. When tether is stopped the marks are gone and a
// placeholder would read as zeros, so an open of a placeholder whose inode
// no fanotify group has marked fails with EIO. Everything else is allowed.
//
// Rebuild with `go generate ./lib/hsm` (needs clang and llvm-strip).
typedef unsigned int __u32;
typedef unsigned long long __u64;
typedef long long __s64;

#define SEC(name) __attribute__((section(name), used))
#define __ksym __attribute__((section(".ksyms")))

struct bpf_dynptr {
	__u64 __opaque[2];
} __attribute__((aligned(8)));

// Only the fields we read; CO-RE relocates them against the running kernel.
struct inode {
	__s64 i_size;
	__u64 i_blocks;
	__u32 i_fsnotify_mask;
} __attribute__((preserve_access_index));

struct file {
	struct inode *f_inode;
} __attribute__((preserve_access_index));

struct task_struct;

// The xattr value is read into per-task storage: dynptrs cannot point at
// the stack, and a per-CPU buffer could be overwritten while this sleepable
// program sleeps inside bpf_get_file_xattr.
struct xattr_buf {
	char v[8];
};

#define __uint(name, val) int (*name)[val]
#define __type(name, val) typeof(val) *name
struct {
	__uint(type, 29);     /* BPF_MAP_TYPE_TASK_STORAGE */
	__uint(map_flags, 1); /* BPF_F_NO_PREALLOC */
	__type(key, int);
	__type(value, struct xattr_buf);
} bufs SEC(".maps");

static void *(*bpf_task_storage_get)(void *map, struct task_struct *task, void *value, __u64 flags) = (void *)156;
static struct task_struct *(*bpf_get_current_task_btf)(void) = (void *)158;

extern int bpf_get_file_xattr(struct file *file, const char *name__str, struct bpf_dynptr *value_p) __ksym;
static long (*bpf_dynptr_from_mem)(void *data, __u32 size, __u64 flags, struct bpf_dynptr *ptr) = (void *)197;

#define FS_OPEN_PERM 0x00010000
#define EIO 5

char LICENSE[] SEC("license") = "GPL";

SEC("lsm.s/file_open")
int tether_guard(__u64 *ctx)
{
	struct file *file = (struct file *)ctx[0];
	int ret = (int)ctx[1];
	if (ret)
		return ret;
	struct inode *inode = file->f_inode;
	if (!inode)
		return 0;
	// A marked inode is served by tether (the open waits for hydration).
	if (inode->i_fsnotify_mask & FS_OPEN_PERM)
		return 0;
	// Placeholders hold no data: at most one block (for xattrs). Only
	// such files pay for the xattr lookup.
	if (inode->i_size <= 0 || inode->i_blocks > 8)
		return 0;
	struct xattr_buf *b = bpf_task_storage_get(&bufs, bpf_get_current_task_btf(), 0, 1 /* F_CREATE */);
	if (!b)
		return 0;
	char *buf = b->v;
	struct bpf_dynptr ptr;
	bpf_dynptr_from_mem(buf, sizeof(b->v), 0, &ptr);
	if (bpf_get_file_xattr(file, "user.tether.state", &ptr) != 7)
		return 0;
	if (buf[0] == 'v' && buf[1] == 'i' && buf[2] == 'r' && buf[3] == 't' && buf[4] == 'u' && buf[5] == 'a' && buf[6] == 'l')
		return -EIO;
	return 0;
}
