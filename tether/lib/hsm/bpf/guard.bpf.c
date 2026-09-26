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
struct hlist_node {
	struct hlist_node *next;
} __attribute__((preserve_access_index));

struct hlist_head {
	struct hlist_node *first;
} __attribute__((preserve_access_index));

struct fsnotify_group {
	_Bool shutdown;
} __attribute__((preserve_access_index));

struct fsnotify_mark {
	__u32 mask;
	struct fsnotify_group *group;
	struct hlist_node obj_list;
} __attribute__((preserve_access_index));

struct fsnotify_mark_connector {
	struct hlist_head list;
} __attribute__((preserve_access_index));

struct inode {
	unsigned int i_flags;
	__s64 i_size;
	__u64 i_blocks;
	__u32 i_fsnotify_mask;
	struct fsnotify_mark_connector *i_fsnotify_marks;
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
static long (*bpf_probe_read_kernel)(void *dst, __u32 size, const void *unsafe_ptr) = (void *)113;

#define offset_of(type, field) __builtin_preserve_field_info(((type *)0)->field, 0 /* BPF_FIELD_BYTE_OFFSET */)
#define read(dst, src) bpf_probe_read_kernel(&(dst), sizeof(dst), &(src))

#define FS_OPEN_PERM 0x00010000
#define S_NOATIME (1 << 1)
#define EIO 5

char LICENSE[] SEC("license") = "GPL";

// served reports whether a live fanotify group has the inode marked for
// open permission events. The inode's fsnotify mask alone is not enough:
// when tether's group closes, its marks are detached asynchronously (about
// 10 ms for 17,000 of them), and until then the mask still says "marked"
// while the dying group raises no events. The group is flagged shutdown
// before that starts. The walk uses probe reads: the marks may be freed
// under us, which at worst yields a wrong answer, never a fault.
static int served(struct inode *inode)
{
	struct fsnotify_mark_connector *conn = 0;
	struct hlist_node *node = 0;
	read(conn, inode->i_fsnotify_marks);
	if (!conn)
		return 0;
	read(node, conn->list.first);
	for (int i = 0; i < 16 && node; i++) {
		struct fsnotify_mark *mark = (struct fsnotify_mark *)((char *)node - offset_of(struct fsnotify_mark, obj_list));
		__u32 mask = 0;
		struct fsnotify_group *group = 0;
		_Bool shutdown = 1;
		read(mask, mark->mask);
		read(group, mark->group);
		if ((mask & FS_OPEN_PERM) && group) {
			read(shutdown, group->shutdown);
			if (!shutdown)
				return 1;
		}
		read(node, node->next);
	}
	return 0;
}

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
	if ((inode->i_fsnotify_mask & FS_OPEN_PERM) && served(inode))
		return 0;
	// Placeholders hold no data: at most one block (for xattrs). Only
	// such files pay for the xattr lookup, and files being hydrated, which
	// carry S_NOATIME (chattr +A) meanwhile: a crash can leave one partly
	// written.
	if (inode->i_size <= 0 || (inode->i_blocks > 8 && !(inode->i_flags & S_NOATIME)))
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
