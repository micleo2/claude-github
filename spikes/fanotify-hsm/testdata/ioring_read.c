// Minimal io_uring IORING_OP_READ of a whole file, printed to stdout.
// Raw syscalls only, so it builds without liburing.
#include <fcntl.h>
#include <linux/io_uring.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

int main(int argc, char **argv) {
	if (argc != 2) { fprintf(stderr, "usage: %s file\n", argv[0]); return 2; }
	int fd = open(argv[1], O_RDONLY);
	if (fd < 0) { perror("open"); return 1; }
	struct stat st; fstat(fd, &st);
	char *buf = malloc(st.st_size + 1);

	struct io_uring_params p; memset(&p, 0, sizeof p);
	int ring = syscall(__NR_io_uring_setup, 4, &p);
	if (ring < 0) { perror("io_uring_setup"); return 1; }
	size_t sqsz = p.sq_off.array + p.sq_entries * sizeof(unsigned);
	size_t cqsz = p.cq_off.cqes + p.cq_entries * sizeof(struct io_uring_cqe);
	char *sq = mmap(0, sqsz, PROT_READ|PROT_WRITE, MAP_SHARED|MAP_POPULATE, ring, IORING_OFF_SQ_RING);
	char *cq = mmap(0, cqsz, PROT_READ|PROT_WRITE, MAP_SHARED|MAP_POPULATE, ring, IORING_OFF_CQ_RING);
	struct io_uring_sqe *sqes = mmap(0, p.sq_entries * sizeof(struct io_uring_sqe), PROT_READ|PROT_WRITE, MAP_SHARED|MAP_POPULATE, ring, IORING_OFF_SQES);

	unsigned *tail = (unsigned *)(sq + p.sq_off.tail);
	unsigned *array = (unsigned *)(sq + p.sq_off.array);
	struct io_uring_sqe *sqe = &sqes[0];
	memset(sqe, 0, sizeof *sqe);
	sqe->opcode = IORING_OP_READ;
	sqe->fd = fd;
	sqe->addr = (unsigned long)buf;
	sqe->len = st.st_size;
	sqe->off = 0;
	array[0] = 0;
	__atomic_store_n(tail, *tail + 1, __ATOMIC_RELEASE);
	if (syscall(__NR_io_uring_enter, ring, 1, 1, IORING_ENTER_GETEVENTS, NULL, 0) < 0) { perror("enter"); return 1; }
	unsigned *chead = (unsigned *)(cq + p.cq_off.head);
	struct io_uring_cqe *cqe = &((struct io_uring_cqe *)(cq + p.cq_off.cqes))[*chead & (p.cq_entries - 1)];
	if (cqe->res < 0) { fprintf(stderr, "read: %s\n", strerror(-cqe->res)); return 1; }
	fwrite(buf, 1, cqe->res, stdout);
	return 0;
}
