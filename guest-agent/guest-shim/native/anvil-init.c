// anvil-init-native — the container init without the Go runtime.
//
// Same two stages and the same report/fd protocol as the Go helper
// (guest-shim/init.go), but the work is data-driven: the agent precomputes an
// "init plan" from the OCI spec (mounts, devices, masked/read-only paths,
// pivot, sysctls, hostname, cwd, credentials, capabilities and the assembled
// seccomp BPF program) and this helper only executes it. Two consequences:
//
//   - no JSON parsing and no seccomp assembly per container (the agent caches
//     the assembled default profile), and
//   - stage B is a plain clone(2) child of this single-threaded process, so it
//     does NOT re-exec a runtime inside the new pid namespace: the process
//     that becomes the container's PID 1 is the user program itself.
//
// Stage A clones stage B with CLONE_NEWPID (and CLONE_INTO_CGROUP when the
// agent passed the cgroup directory as fd 8), reports "P <pid>", waits for
// the confirm pipe (EOF = the user exec landed) and reports "S"/"D". Stage B
// joins the spec's namespaces, runs the op list, reports "R", holds at the
// gate (fd 3), applies the process attributes (seccomp strictly last) and
// execve's the user process.
//
// The Go helper remains the fallback; the agent selects the implementation
// per container and falls back on any plan it cannot express.
//
// Fixed descriptors (the agent's ExtraFiles start at 3):
//   3 gate (read), 4 report (write), 5/6/7 stdin/stdout/stderr,
//   8 cgroup directory (CLONE_INTO_CGROUP), optional.
//
// Plan format (all integers little-endian; strings are u32 length, the bytes,
// then a NUL byte): a 12-byte header (magic "ANPL", version, total length),
// then sections {u32 type, u32 length, payload}. See guest-agent/init_plan.go
// for the writer and the section/op codes.

#define _GNU_SOURCE

#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <sched.h>
#include <signal.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/prctl.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#ifndef CLONE_INTO_CGROUP
#define CLONE_INTO_CGROUP (1ULL << 33)
#endif
#ifndef SYS_clone3
#define SYS_clone3 435
#endif
#ifndef SECCOMP_SET_MODE_FILTER
#define SECCOMP_SET_MODE_FILTER 1
#endif
#ifndef SECCOMP_RET_ALLOW
#define SECCOMP_RET_ALLOW 0x7fff0000U
#endif
#ifndef PR_SET_OOM_SCORE_ADJ
#define PR_SET_OOM_SCORE_ADJ 36
#endif
#ifndef PR_SET_NO_NEW_PRIVS
#define PR_SET_NO_NEW_PRIVS 38
#endif
#ifndef PR_SET_KEEPCAPS
#define PR_SET_KEEPCAPS 8
#endif
#ifndef PR_CAPBSET_DROP
#define PR_CAPBSET_DROP 24
#endif
#ifndef PR_CAP_AMBIENT
#define PR_CAP_AMBIENT 47
#define PR_CAP_AMBIENT_RAISE 2
#endif

// struct sock_fprog/sock_filter without linux/filter.h (musl-dev does not
// ship the kernel headers; the layout is a stable UAPI).
struct anvil_sock_filter {
	uint16_t code;
	uint8_t jt;
	uint8_t jf;
	uint32_t k;
};
struct anvil_sock_fprog {
	uint16_t len;
	struct anvil_sock_filter *filter;
};

// Fixed fds handed over by the agent (os/exec ExtraFiles start at 3).
enum {
	FD_GATE = 3,
	FD_REPORT = 4,
	FD_STDIN = 5,
	FD_STDOUT = 6,
	FD_STDERR = 7,
	FD_CGROUP = 8,
	FD_CONFIRM = 60, // confirm pipe write end, cleared close-on-exec
	FD_REPORT_FINAL = 61,
};

#define PLAN_MAGIC 0x4c504e41u // "ANPL"
#define PLAN_VERSION 1

enum {
	SEC_CGROUP = 1,
	SEC_OP = 5,
	SEC_ARGV = 6,
	SEC_ENV = 7,
	SEC_NS = 8,
	SEC_ATTRS = 9,
};

enum {
	OP_MKDIRALL = 1,
	OP_TOUCH,
	OP_MOUNT,
	OP_MKNOD,
	OP_MKNOD_OPT,
	OP_SYMLINK,
	OP_REMOVE,
	OP_WRITE,
	OP_MASK,
	OP_READONLY,
	OP_PIVOT,
	OP_ROOT_RO,
	OP_SETHOSTNAME,
	OP_CHDIR,
};

#define MAX_OPS 256
#define MAX_NS 32
#define MAX_ARGS 64
#define MAX_ENV 128
#define MAX_RLIMITS 32
#define MAX_GIDS 64

struct op {
	uint32_t kind;
	uint32_t flags;
	uint32_t mode;
	uint64_t dev;
	const char *s1, *s2, *s3, *s4;
};

struct ns_ent {
	uint32_t flag;
	const char *path; // NULL: pathless (unshare)
};

struct rlim {
	uint32_t resource;
	uint64_t soft, hard;
};

struct plan {
	struct op ops[MAX_OPS];
	int nops;
	struct ns_ent ns[MAX_NS];
	int nns;
	const char *cgroup_path; // removed at container exit, like the Go helper
	const char *argv[MAX_ARGS + 1];
	int argc;
	const char *envp[MAX_ENV + 1];
	int envc;
	int has_oom, oom;
	int nnps;
	int nonroot;
	uint32_t uid, gid;
	struct rlim rlims[MAX_RLIMITS];
	int nrlims;
	uint32_t gids[MAX_GIDS];
	int ngids;
	int has_caps;
	uint64_t caps[5]; // bounding, permitted, effective, inheritable, ambient
	uint64_t *seccomp; // encoded instructions
	int nseccomp;
	uint8_t *buf; // the mapped plan (kept for the string pointers)
};

static struct plan P;

// --- reporting -------------------------------------------------------------

static int write_all(int fd, const char *data, size_t len);

// --- latency trace ---------------------------------------------------------
//
// The same per-phase trace the Go helper writes (guest-shim/init.go): one
// line "<id> <stage> <wall-ns> <thread-cpu-ns>" per mark, on tmpfs, only when
// ANVIL_HELPER_TRACE is set (debug builds). The agent copies the setup file
// when it sees "R" and the "-late" file asynchronously at "S"; the late file
// is opened before pivot_root because "/tmp" afterwards is the container's.
static const char *g_id;
static int g_trace_setup = -1, g_trace_late = -1;

static int trace_open(const char *suffix)
{
	char path[128];
	snprintf(path, sizeof path, "/tmp/anvil-helper-trace-%s%s", suffix, g_id);
	return open(path, O_CREAT | O_WRONLY | O_APPEND | O_CLOEXEC, 0644);
}

static void trace_mark(int late, const char *stage)
{
	if (!g_id || !getenv("ANVIL_HELPER_TRACE"))
		return;
	int *slot = late ? &g_trace_late : &g_trace_setup;
	if (*slot < 0) {
		*slot = trace_open(late ? "late-" : "");
		if (*slot < 0)
			return;
	}
	struct timespec w, c;
	if (clock_gettime(CLOCK_REALTIME, &w) != 0)
		return;
	int64_t cpu = -1;
	if (clock_gettime(CLOCK_THREAD_CPUTIME_ID, &c) == 0)
		cpu = (int64_t)c.tv_sec * 1000000000 + c.tv_nsec;
	char line[160];
	int n = snprintf(line, sizeof line, "%s %s %lld %lld\n", g_id, stage,
			 (long long)w.tv_sec * 1000000000 + w.tv_nsec, (long long)cpu);
	if (n > 0)
		write_all(*slot, line, (size_t)n);
}

static void report_to(int fd, const char *fmt, ...)
{
	char buf[512];
	va_list ap;
	va_start(ap, fmt);
	int n = vsnprintf(buf, sizeof buf, fmt, ap);
	va_end(ap);
	if (n < 0)
		return;
	if ((size_t)n >= sizeof buf)
		n = (int)sizeof buf - 1;
	if (n > 0 && buf[n - 1] != '\n' && (size_t)n + 1 < sizeof buf) {
		buf[n++] = '\n';
		buf[n] = 0;
	}
	ssize_t off = 0;
	while (off < n) {
		ssize_t w = write(fd, buf + off, (size_t)(n - off));
		if (w <= 0)
			return;
		off += w;
	}
}

static void report(const char *fmt, ...)
{
	char buf[512];
	va_list ap;
	va_start(ap, fmt);
	int n = vsnprintf(buf, sizeof buf, fmt, ap);
	va_end(ap);
	if (n < 0)
		return;
	if ((size_t)n >= sizeof buf)
		n = (int)sizeof buf - 1;
	if (n > 0 && buf[n - 1] != '\n' && (size_t)n + 1 < sizeof buf) {
		buf[n++] = '\n';
		buf[n] = 0;
	}
	ssize_t off = 0;
	while (off < n) {
		ssize_t w = write(FD_REPORT, buf + off, (size_t)(n - off));
		if (w <= 0)
			return;
		off += w;
	}
}

// g_where carries the last failing step; g_err the failure reason. failf
// captures errno at entry: the formatting itself may clobber it.
static char g_where[256];
static int g_err;

static int failf(const char *fmt, ...)
{
	g_err = errno;
	va_list ap;
	va_start(ap, fmt);
	vsnprintf(g_where, sizeof g_where, fmt, ap);
	va_end(ap);
	return -1;
}

static const char *errno_name(int e)
{
	switch (e) {
	case EPERM: return "EPERM";
	case ENOENT: return "ENOENT";
	case EINVAL: return "EINVAL";
	case EEXIST: return "EEXIST";
	case EACCES: return "EACCES";
	case ENOSYS: return "ENOSYS";
	case ENOMEM: return "ENOMEM";
	case ENOTDIR: return "ENOTDIR";
	case EISDIR: return "EISDIR";
	case EBUSY: return "EBUSY";
	case EROFS: return "EROFS";
	case EOPNOTSUPP: return "EOPNOTSUPP";
	default: return "error";
	}
}

// --- plan parsing ----------------------------------------------------------

struct reader {
	const uint8_t *p, *end;
};

static int take_u32(struct reader *r, uint32_t *out)
{
	if (r->end - r->p < 4)
		return -1;
	*out = (uint32_t)r->p[0] | (uint32_t)r->p[1] << 8 |
	       (uint32_t)r->p[2] << 16 | (uint32_t)r->p[3] << 24;
	r->p += 4;
	return 0;
}

static int take_u64(struct reader *r, uint64_t *out)
{
	uint32_t lo, hi;
	if (take_u32(r, &lo) || take_u32(r, &hi))
		return -1;
	*out = (uint64_t)lo | (uint64_t)hi << 32;
	return 0;
}

// take_str returns a pointer into the plan; the string is NUL-terminated there.
static int take_str(struct reader *r, const char **out)
{
	uint32_t n;
	if (take_u32(r, &n))
		return -1;
	if ((size_t)(r->end - r->p) < (size_t)n + 1)
		return -1;
	const char *s = (const char *)r->p;
	if (s[n] != 0)
		return -1;
	r->p += (size_t)n + 1;
	*out = s;
	return 0;
}

static int parse_op(struct reader *r)
{
	if (P.nops >= MAX_OPS)
		return -1;
	struct op *o = &P.ops[P.nops];
	memset(o, 0, sizeof *o);
	if (take_u32(r, &o->kind))
		return -1;
	switch (o->kind) {
	case OP_MKDIRALL:
	case OP_TOUCH:
	case OP_REMOVE:
	case OP_CHDIR:
	case OP_MASK:
	case OP_READONLY:
	case OP_PIVOT:
	case OP_SETHOSTNAME:
		if (take_str(r, &o->s1))
			return -1;
		break;
	case OP_MOUNT:
		if (take_u32(r, &o->flags) || take_str(r, &o->s1) ||
		    take_str(r, &o->s2) || take_str(r, &o->s3) || take_str(r, &o->s4))
			return -1;
		break;
	case OP_MKNOD:
	case OP_MKNOD_OPT:
		if (take_u32(r, &o->mode) || take_u64(r, &o->dev) || take_str(r, &o->s1))
			return -1;
		break;
	case OP_SYMLINK:
		if (take_str(r, &o->s1) || take_str(r, &o->s2))
			return -1;
		break;
	case OP_WRITE:
		if (take_str(r, &o->s1) || take_str(r, &o->s2))
			return -1;
		break;
	case OP_ROOT_RO:
		break;
	default:
		return -1;
	}
	P.nops++;
	return 0;
}

static int parse_attrs(struct reader *r)
{
	uint32_t v;
	int32_t sv;
	if (take_u32(r, &v)) return -1;
	P.has_oom = (int)v;
	// oom_score_adj is a signed int in the OCI spec.
	if (take_u32(r, &v)) return -1;
	memcpy(&sv, &v, sizeof sv);
	P.oom = sv;
	if (take_u32(r, &v)) return -1;
	P.nnps = (int)v;
	if (take_u32(r, &v)) return -1;
	P.nonroot = (int)v;
	if (take_u32(r, &P.uid) || take_u32(r, &P.gid))
		return -1;
	if (take_u32(r, &v) || v > MAX_RLIMITS)
		return -1;
	P.nrlims = (int)v;
	for (int i = 0; i < P.nrlims; i++) {
		if (take_u32(r, &P.rlims[i].resource) ||
		    take_u64(r, &P.rlims[i].soft) || take_u64(r, &P.rlims[i].hard))
			return -1;
	}
	if (take_u32(r, &v) || v > MAX_GIDS)
		return -1;
	P.ngids = (int)v;
	for (int i = 0; i < P.ngids; i++)
		if (take_u32(r, &P.gids[i]))
			return -1;
	if (take_u32(r, &v))
		return -1;
	P.has_caps = (int)v;
	for (int i = 0; i < 5; i++)
		if (take_u64(r, &P.caps[i]))
			return -1;
	if (take_u32(r, &v) || v > 1 << 20)
		return -1;
	P.nseccomp = (int)v;
	if (P.nseccomp) {
		size_t bytes = (size_t)P.nseccomp * 8;
		if ((size_t)(r->end - r->p) < bytes)
			return -1;
		P.seccomp = malloc(bytes);
		if (!P.seccomp)
			return -1;
		memcpy(P.seccomp, r->p, bytes);
		r->p += bytes;
	}
	return 0;
}

static int load_plan(const char *path)
{
	FILE *f = fopen(path, "rb");
	if (!f)
		return failf("open init plan %s", path);
	if (fseek(f, 0, SEEK_END) != 0) {
		fclose(f);
		return failf("seek init plan");
	}
	long sz = ftell(f);
	if (sz <= 0 || sz > (1 << 22) || fseek(f, 0, SEEK_SET) != 0) {
		fclose(f);
		return failf("size init plan");
	}
	P.buf = malloc((size_t)sz);
	if (!P.buf) {
		fclose(f);
		return failf("alloc init plan");
	}
	if (fread(P.buf, 1, (size_t)sz, f) != (size_t)sz) {
		fclose(f);
		return failf("read init plan");
	}
	fclose(f);

	struct reader r = {P.buf, P.buf + sz};
	uint32_t magic, version, total;
	if (take_u32(&r, &magic) || take_u32(&r, &version) || take_u32(&r, &total))
		return failf("plan header truncated");
	if (magic != PLAN_MAGIC || version != PLAN_VERSION || total != (uint32_t)sz)
		return failf("plan header invalid");
	while (r.p < r.end) {
		uint32_t type, len;
		if (take_u32(&r, &type) || take_u32(&r, &len))
			return failf("plan section header truncated");
		if ((size_t)(r.end - r.p) < len)
			return failf("plan section %u truncated", type);
		struct reader s = {r.p, r.p + len};
		int rc = 0;
		switch (type) {
		case SEC_CGROUP:
			rc = take_str(&s, &P.cgroup_path);
			break;
		case SEC_OP:
			rc = parse_op(&s);
			break;
		case SEC_NS:
			if (P.nns >= MAX_NS ||
			    take_u32(&s, &P.ns[P.nns].flag)) {
				rc = -1;
				break;
			}
			uint32_t has_path;
			if (take_u32(&s, &has_path)) {
				rc = -1;
				break;
			}
			if (has_path && take_str(&s, &P.ns[P.nns].path)) {
				rc = -1;
				break;
			}
			P.nns++;
			break;
		case SEC_ARGV:
		case SEC_ENV: {
			const char **arr = type == SEC_ARGV ? P.argv : P.envp;
			int *n = type == SEC_ARGV ? &P.argc : &P.envc;
			int max = type == SEC_ARGV ? MAX_ARGS : MAX_ENV;
			while (s.p < s.end) {
				if (*n >= max) {
					rc = -1;
					break;
				}
				if (take_str(&s, &arr[*n])) {
					rc = -1;
					break;
				}
				(*n)++;
			}
			arr[*n] = NULL;
			break;
		}
		case SEC_ATTRS:
			rc = parse_attrs(&s);
			break;
		default:
			break; // unknown sections are ignored (forward compatible)
		}
		if (rc)
			return failf("plan section %u malformed", type);
		r.p += len;
	}
	if (P.argc == 0)
		return failf("plan has no argv");
	return 0;
}

// --- filesystem helpers ----------------------------------------------------

static int mkdir_p(const char *path, mode_t mode)
{
	// One stat when the tree already exists, like Go's os.MkdirAll: the
	// mountpoints sit under an existing rootfs, so the common case must not
	// walk (and mkdir) every component.
	struct stat st;
	if (stat(path, &st) == 0) {
		if (S_ISDIR(st.st_mode))
			return 0;
		return failf("mkdir %s: not a directory", path);
	}
	char tmp[PATH_MAX];
	size_t n = strlen(path);
	if (n >= sizeof tmp)
		return failf("path too long: %s", path);
	memcpy(tmp, path, n + 1);
	for (char *p = tmp + 1; *p; p++) {
		if (*p != '/')
			continue;
		*p = 0;
		if (mkdir(tmp, mode) != 0 && errno != EEXIST)
			return failf("mkdir %s", tmp);
		*p = '/';
	}
	if (mkdir(tmp, mode) != 0 && errno != EEXIST)
		return failf("mkdir %s", tmp);
	return 0;
}

static int rm_rf(const char *path)
{
	struct stat st;
	if (lstat(path, &st) != 0)
		return 0;
	if (S_ISDIR(st.st_mode)) {
		DIR *d = opendir(path);
		if (d) {
			struct dirent *e;
			while ((e = readdir(d)) != NULL) {
				if (strcmp(e->d_name, ".") == 0 || strcmp(e->d_name, "..") == 0)
					continue;
				char p[PATH_MAX];
				if (snprintf(p, sizeof p, "%s/%s", path, e->d_name) < (int)sizeof p)
					rm_rf(p);
			}
			closedir(d);
		}
		return rmdir(path);
	}
	return unlink(path);
}

static int write_all(int fd, const char *data, size_t len)
{
	size_t off = 0;
	while (off < len) {
		ssize_t w = write(fd, data + off, len - off);
		if (w < 0) {
			if (errno == EINTR)
				continue;
			return -1;
		}
		off += (size_t)w;
	}
	return 0;
}

// --- stage B: namespaces and ops -------------------------------------------

struct open_ns {
	int fd;
	int mount;
};

static int join_namespaces(void)
{
	struct open_ns opened[MAX_NS];
	int nopen = 0;

	// Open every pathed namespace first: paths still resolve in the guest's
	// mount namespace view. Mount namespaces are switched last, like runc.
	for (int i = 0; i < P.nns; i++) {
		if (!P.ns[i].path)
			continue;
		if (P.ns[i].flag == CLONE_NEWPID)
			return failf("pid namespace joined by path is not supported");
		if (P.ns[i].flag == CLONE_NEWNS)
			return failf("mount namespace joined by path is not supported");
		int fd = open(P.ns[i].path, O_RDONLY | O_CLOEXEC);
		if (fd < 0)
			return failf("open namespace %s", P.ns[i].path);
		opened[nopen].fd = fd;
		opened[nopen].mount = 0;
		nopen++;
	}
	for (int i = 0; i < nopen; i++) {
		if (syscall(SYS_setns, opened[i].fd, 0) != 0) {
			int e = errno;
			for (int j = 0; j < nopen; j++)
				close(opened[j].fd);
			errno = e;
			return failf("setns");
		}
	}
	for (int i = 0; i < nopen; i++)
		close(opened[i].fd);

	// Pathless namespaces: unshare the whole set at once. The pid namespace
	// is skipped (the clone already put this process inside it); the mount
	// namespace is included here when pathless.
	uint64_t flags = 0;
	for (int i = 0; i < P.nns; i++) {
		if (P.ns[i].path || P.ns[i].flag == CLONE_NEWPID)
			continue;
		flags |= P.ns[i].flag;
	}
	if (flags && syscall(SYS_unshare, flags) != 0)
		return failf("unshare namespaces");
	return 0;
}

static int pivot_root_into(const char *rootfs)
{
	if (mount("", "/", "", MS_REC | MS_PRIVATE, "") != 0)
		return failf("make / rprivate");

	char old_root[PATH_MAX];
	if (snprintf(old_root, sizeof old_root, "%s/.pivot_root", rootfs) >= (int)sizeof old_root)
		return failf("pivot path too long");
	rm_rf(old_root);
	if (mkdir(old_root, 0700) != 0)
		return failf("mkdir pivot point");

	if (syscall(SYS_pivot_root, rootfs, old_root) != 0) {
		if (errno != EINVAL)
			return failf("pivot_root %s", rootfs);
		// rootfs is not a separate mount: move it over / and chroot, runc's
		// fallback.
		if (mount(rootfs, "/", "", MS_MOVE, "") != 0)
			return failf("MS_MOVE rootfs");
		if (chroot(".") != 0)
			return failf("chroot after MS_MOVE");
		if (chdir("/") != 0)
			return failf("chdir /");
		return 0;
	}
	if (chdir("/") != 0)
		return failf("chdir / after pivot");
	if (umount2("/.pivot_root", MNT_DETACH) != 0) {
		// Diagnostics only; the old root is detached lazily enough.
	}
	rm_rf("/.pivot_root");
	return 0;
}

static int write_file(const char *path, const char *data)
{
	int fd = open(path, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0644);
	if (fd < 0)
		return failf("open %s", path);
	int rc = write_all(fd, data, strlen(data));
	int e = errno;
	close(fd);
	if (rc) {
		errno = e;
		return failf("write %s", path);
	}
	return 0;
}

static int run_ops(void)
{
	int mounts_done = 0, devices_done = 0, masked_done = 0;
	for (int i = 0; i < P.nops; i++) {
		struct op *o = &P.ops[i];
		// Group boundaries for the latency trace (mounts, devices,
		// masked/readonly, pivot, the rest), matching the Go helper's marks.
		if (!mounts_done && o->kind != OP_MKDIRALL && o->kind != OP_TOUCH && o->kind != OP_MOUNT) {
			trace_mark(1, "B-mounts");
			mounts_done = 1;
		}
		if (!devices_done && (o->kind == OP_MASK || o->kind == OP_READONLY)) {
			trace_mark(1, "B-devices");
			devices_done = 1;
		}
		if (!masked_done && o->kind == OP_PIVOT) {
			trace_mark(1, "B-masked");
			masked_done = 1;
		}
		switch (o->kind) {
		case OP_MKDIRALL:
			if (mkdir_p(o->s1, 0755))
				return -1;
			break;
		case OP_TOUCH: {
			// Create the mountpoint file only when it is missing, like the Go
			// helper (os.Stat + os.OpenFile). Opening an existing lower-layer
			// file for write triggers an overlay copy-up: that is a disk
			// write per bind-mounted file (measured ~0.4-0.7 ms each on the
			// dev guest) for a file that does not need to change.
			struct stat st;
			if (stat(o->s1, &st) == 0)
				break;
			int fd = open(o->s1, O_CREAT | O_WRONLY | O_CLOEXEC, 0644);
			if (fd < 0)
				return failf("create mountpoint %s", o->s1);
			close(fd);
			break;
		}
		case OP_MOUNT:
			if (mount(o->s1, o->s2, o->s3, o->flags, o->s4) != 0)
				return failf("mount %s on %s", o->s3, o->s2);
			break;
		case OP_MKNOD:
			if (mknod(o->s1, (mode_t)o->mode, (dev_t)o->dev) != 0 && errno != EEXIST)
				return failf("mknod %s", o->s1);
			break;
		case OP_MKNOD_OPT:
			mknod(o->s1, (mode_t)o->mode, (dev_t)o->dev); // best effort
			break;
		case OP_SYMLINK:
			unlink(o->s2);
			if (symlink(o->s1, o->s2) != 0 && errno != EEXIST)
				return failf("symlink %s", o->s2);
			break;
		case OP_REMOVE:
			unlink(o->s1);
			break;
		case OP_WRITE:
			if (write_file(o->s1, o->s2))
				return -1;
			break;
		case OP_MASK: {
			struct stat st;
			if (stat(o->s1, &st) != 0)
				break; // nothing to mask
			mount("/dev/null", o->s1, "", MS_BIND, ""); // diagnostics only
			break;
		}
		case OP_READONLY: {
			struct stat st;
			if (stat(o->s1, &st) != 0)
				break;
			if (mount(o->s1, o->s1, "", MS_BIND | MS_REC, "") != 0)
				break;
			mount("", o->s1, "", MS_BIND | MS_REC | MS_RDONLY | MS_REMOUNT, "");
			break;
		}
		case OP_PIVOT:
			if (pivot_root_into(o->s1))
				return -1;
			trace_mark(1, "B-pivot");
			break;
		case OP_ROOT_RO:
			if (mount("", "/", "", MS_BIND | MS_REMOUNT | MS_RDONLY, "") != 0)
				return failf("readonly root");
			break;
		case OP_SETHOSTNAME:
			if (sethostname(o->s1, strlen(o->s1)) != 0)
				return failf("sethostname");
			break;
		case OP_CHDIR:
			if (chdir(o->s1) != 0)
				return failf("chdir %s", o->s1);
			break;
		default:
			return failf("unknown plan op %u", o->kind);
		}
	}
	return 0;
}

// --- stage B: attributes ---------------------------------------------------

static int drop_bounding_caps(uint64_t bound)
{
	for (int c = 0; c < 64; c++) {
		if (bound & (1ULL << c))
			continue;
		if (prctl(PR_CAPBSET_DROP, c, 0, 0, 0) != 0 && errno != EINVAL)
			return failf("drop bounding cap %d", c);
	}
	return 0;
}

static int apply_capabilities(void)
{
	uint64_t perm = P.caps[1];
	if (perm == 0)
		perm = P.caps[0]; // runc: fall back to bounding
	uint64_t eff = P.caps[2];
	if (eff == 0)
		eff = perm;
	uint64_t inh = P.caps[3];
	uint64_t amb = P.caps[4];

	struct {
		uint32_t version;
		int pid;
	} hdr = {0x20080522u, 0};
	struct {
		uint32_t effective, permitted, inheritable;
	} data[2] = {
		{(uint32_t)eff, (uint32_t)perm, (uint32_t)inh},
		{(uint32_t)(eff >> 32), (uint32_t)(perm >> 32), (uint32_t)(inh >> 32)},
	};
	if (P.nonroot) {
		struct {
			uint32_t effective, permitted, inheritable;
		} raised[2] = {
			{(uint32_t)perm, (uint32_t)perm, (uint32_t)inh},
			{(uint32_t)(perm >> 32), (uint32_t)(perm >> 32), (uint32_t)(inh >> 32)},
		};
		if (syscall(SYS_capset, &hdr, raised) != 0)
			return failf("capset (raise)");
	} else if (drop_bounding_caps(P.caps[0])) {
		return -1;
	}
	if (syscall(SYS_capset, &hdr, data) != 0)
		return failf("capset");
	for (int c = 0; c < 64; c++) {
		if (!(amb & (1ULL << c)))
			continue;
		if (prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_RAISE, c, 0, 0) != 0)
			return failf("ambient cap %d", c);
	}
	return 0;
}

static int apply_attrs(void)
{
	if (P.has_oom && prctl(PR_SET_OOM_SCORE_ADJ, P.oom, 0, 0, 0) != 0)
		return failf("oom score adj");
	for (int i = 0; i < P.nrlims; i++) {
		struct rlimit rl = {P.rlims[i].soft, P.rlims[i].hard};
		if (setrlimit(P.rlims[i].resource, &rl) != 0)
			return failf("rlimit %u", P.rlims[i].resource);
	}
	if (P.nnps && prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0)
		return failf("no new privs");
	// Credentials and capabilities are interdependent: drop the bounding set
	// while still root, carry the permitted set across the setuid, then
	// install the spec's sets (runc's order).
	if (P.has_caps && P.nonroot) {
		if (drop_bounding_caps(P.caps[0]))
			return -1;
		if (prctl(PR_SET_KEEPCAPS, 1, 0, 0, 0) != 0)
			return failf("keep caps");
	}
	if (P.nonroot) {
		if (P.ngids > 0 && setgroups((size_t)P.ngids, (const gid_t *)P.gids) != 0)
			return failf("setgroups");
		if (setresgid(P.gid, P.gid, P.gid) != 0)
			return failf("setgid %u", P.gid);
		if (setresuid(P.uid, P.uid, P.uid) != 0)
			return failf("setuid %u", P.uid);
	}
	if (P.has_caps && apply_capabilities())
		return -1;
	if (P.has_caps && P.nonroot && prctl(PR_SET_KEEPCAPS, 0, 0, 0, 0) != 0)
		return failf("clear keep caps");
	if (P.nseccomp) {
		// The agent assembled the filter and needs no CAP_SYS_ADMIN via
		// no-new-privileges (the default capability set has none).
		if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0)
			return failf("set no_new_privs");
		struct anvil_sock_filter *filt = malloc((size_t)P.nseccomp * sizeof *filt);
		if (!filt)
			return failf("alloc seccomp");
		for (int i = 0; i < P.nseccomp; i++) {
			uint64_t v = P.seccomp[i];
			filt[i].code = (unsigned short)(v & 0xffff);
			filt[i].jt = (unsigned char)((v >> 16) & 0xff);
			filt[i].jf = (unsigned char)((v >> 24) & 0xff);
			filt[i].k = (uint32_t)(v >> 32);
		}
		struct anvil_sock_fprog prog = {.len = (unsigned short)P.nseccomp, .filter = filt};
		// Single-threaded: no TSYNC needed (the Go helper's process is
		// multi-threaded, its filter carries the flag).
		long rc = syscall(SYS_seccomp, SECCOMP_SET_MODE_FILTER, 0, &prog);
		int e = errno;
		free(filt);
		if (rc != 0) {
			errno = e;
			return failf("load seccomp filter");
		}
	}
	return 0;
}

// --- stage B: exec path ----------------------------------------------------

static int is_executable(const char *path)
{
	struct stat st;
	if (stat(path, &st) != 0)
		return 0;
	return !S_ISDIR(st.st_mode) && (st.st_mode & 0111) != 0;
}

static const char *resolve_exec_path(void)
{
	const char *arg0 = P.argv[0];
	if (strchr(arg0, '/'))
		return arg0;
	const char *path_env = NULL;
	for (int i = 0; i < P.envc; i++) {
		if (strncmp(P.envp[i], "PATH=", 5) == 0) {
			path_env = P.envp[i] + 5;
			break;
		}
	}
	static char cand[PATH_MAX];
	if (!path_env)
		return NULL;
	const char *p = path_env;
	for (;;) {
		const char *colon = strchr(p, ':');
		size_t dlen = colon ? (size_t)(colon - p) : strlen(p);
		const char *dir = p;
		char dot[] = ".";
		if (dlen == 0) {
			dir = dot;
			dlen = 1;
		}
		int n = snprintf(cand, sizeof cand, "%.*s/%s", (int)dlen, dir, arg0);
		if (n > 0 && (size_t)n < sizeof cand && is_executable(cand))
			return cand;
		if (!colon)
			break;
		p = colon + 1;
	}
	return NULL;
}

// --- stages ----------------------------------------------------------------

// remove_cgroup deletes the container's cgroup at exit. Frozen state is lifted
// first (the Go helper does the same); a non-empty directory is left alone
// rather than risking a recursive delete outside the agent.
static void remove_cgroup(void)
{
	if (!P.cgroup_path)
		return;
	char p[PATH_MAX];
	if (snprintf(p, sizeof p, "%s/cgroup.freeze", P.cgroup_path) < (int)sizeof p) {
		int fd = open(p, O_WRONLY | O_CLOEXEC);
		if (fd >= 0) {
			if (write(fd, "0", 1) < 0) {
				// Best effort; the rmdir below reports the consequence.
			}
			close(fd);
		}
	}
	rmdir(P.cgroup_path);
}

static int gate_wait(void)
{
	char buf[8];
	for (;;) {
		ssize_t n = read(FD_GATE, buf, sizeof buf);
		if (n <= 0)
			return 0; // EOF: deleted while created
		for (ssize_t i = 0; i < n; i++)
			if (buf[i] == 's')
				return 1;
	}
}

static int stage_b(void)
{
	// The late trace file must be opened before the namespace and mount work:
	// its marks are written after pivot_root, when "/tmp" is the container's.
	trace_mark(1, "B-entry");
	// Final stdio: the agent's pipes/fifo become 0/1/2. They may carry
	// O_NONBLOCK from the agent's side; the user process must see blocking
	// descriptors.
	if (dup2(FD_STDIN, 0) < 0 || dup2(FD_STDOUT, 1) < 0 || dup2(FD_STDERR, 2) < 0) {
		report("E dup stdio: %s", errno_name(errno));
		return 1;
	}
	close(FD_STDIN);
	close(FD_STDOUT);
	close(FD_STDERR);
	for (int fd = 0; fd <= 2; fd++) {
		int fl = fcntl(fd, F_GETFL);
		if (fl >= 0)
			fcntl(fd, F_SETFL, fl & ~O_NONBLOCK);
	}
	// The confirm fd must die at the user exec (its closure is the start
	// confirmation stage A waits for).
	fcntl(FD_CONFIRM, F_SETFD, FD_CLOEXEC);
	// The inherited cgroup directory is no longer needed: the clone already
	// placed this process.
	close(FD_CGROUP);

	if (join_namespaces()) {
		report("E %s: %s", g_where, errno_name(g_err));
		return 1;
	}
	trace_mark(1, "B-ns");
	if (run_ops()) {
		report("E %s: %s", g_where, errno_name(g_err));
		return 1;
	}
	trace_mark(1, "B-ops");
	trace_mark(1, "B-ready");
	report("R");

	if (!gate_wait())
		return 0;
	trace_mark(1, "B-gate");
	close(FD_GATE);

	const char *argv0 = resolve_exec_path();
	if (!argv0) {
		report("X exec %s: executable file not found in $PATH", P.argv[0]);
		return 127;
	}
	trace_mark(1, "B-resolve");
	if (apply_attrs()) {
		report("X %s: %s", g_where, errno_name(g_err));
		return 126;
	}
	trace_mark(1, "B-attrs");
	// dup3 returns the new descriptor on success (not 0): compare < 0.
	if (syscall(SYS_dup3, FD_REPORT, FD_REPORT_FINAL, O_CLOEXEC) < 0) {
		report("X dup report: %s", errno_name(errno));
		return 126;
	}
	close(FD_REPORT);
	trace_mark(1, "B-exec");
	execve(argv0, (char *const *)P.argv, (char *const *)P.envp);
	report_to(FD_REPORT_FINAL, "X exec %s: %s", argv0, errno_name(errno));
	return 127;
}

struct anvil_clone_args {
	uint64_t flags, pidfd, child_tid, parent_tid, exit_signal, stack,
		stack_size, tls, set_tid, set_tid_size, cgroup;
};

// cgroup_move_child writes the child's pid into the cgroup directory's
// cgroup.procs through the inherited directory fd. Only the fallback path
// needs it (CLONE_INTO_CGROUP places the child at birth); it runs in the
// parent before the born pipe releases stage B.
static int cgroup_move_child(int cgroup_fd, pid_t child)
{
	int fd = openat(cgroup_fd, "cgroup.procs", O_WRONLY | O_CLOEXEC);
	if (fd < 0)
		return failf("open cgroup.procs");
	char buf[32];
	int n = snprintf(buf, sizeof buf, "%d", (int)child);
	int rc = write_all(fd, buf, (size_t)n);
	int e = errno;
	close(fd);
	if (rc) {
		errno = e;
		return failf("write cgroup.procs");
	}
	return 0;
}

// clone_stage_b creates stage B inside a fresh pid namespace. With a cgroup
// directory fd it uses clone3 + CLONE_INTO_CGROUP; a kernel without clone3
// falls back to clone(2) and the parent moves the child into the cgroup.
// used_clone3 reports which path ran (the probe requires clone3).
static int clone_stage_b(int cgroup_fd, int *errp, int *used_clone3)
{
	if (used_clone3)
		*used_clone3 = 0;
	if (cgroup_fd >= 0) {
		struct anvil_clone_args args;
		memset(&args, 0, sizeof args);
		args.flags = CLONE_NEWPID | CLONE_INTO_CGROUP;
		args.exit_signal = SIGCHLD;
		args.cgroup = (uint64_t)cgroup_fd;
		long rc = syscall(SYS_clone3, &args, sizeof args);
		if (rc >= 0) {
			if (used_clone3)
				*used_clone3 = 1;
			return (int)rc;
		}
		if (errno != ENOSYS && errno != EINVAL && errno != EOPNOTSUPP) {
			*errp = errno;
			return -1;
		}
	}
	long rc = syscall(SYS_clone, SIGCHLD | CLONE_NEWPID, 0, 0, 0, 0);
	if (rc < 0) {
		*errp = errno;
		return -1;
	}
	if (rc > 0 && cgroup_fd >= 0 && cgroup_move_child(cgroup_fd, (pid_t)rc)) {
		*errp = g_err;
		return -1;
	}
	return (int)rc;
}

static int run_init(const char *plan_path, int have_cgroup)
{
	trace_mark(0, "C-entry");
	if (load_plan(plan_path)) {
		report("E %s", g_where);
		return 1;
	}
	trace_mark(0, "C-plan");
	int confirm[2];
	if (pipe2(confirm, O_CLOEXEC) != 0) {
		report("E confirm pipe: %s", errno_name(errno));
		return 1;
	}
	// born orders the reports: the parent must emit "P <pid>" before the child
	// can emit "R". The Go helper gets that order for free (its stage B is an
	// exec), but a clone(2) child continues immediately and could otherwise
	// reach the ready report first, leaving the agent without the init pid.
	int born[2];
	if (pipe2(born, O_CLOEXEC) != 0) {
		report("E born pipe: %s", errno_name(errno));
		return 1;
	}
	int cgroup_fd = have_cgroup ? FD_CGROUP : -1;
	int err = 0, used_clone3 = 0;
	pid_t child = clone_stage_b(cgroup_fd, &err, &used_clone3);
	if (child < 0) {
		report("E clone stage B: %s", errno_name(err));
		return 1;
	}
	if (child == 0) {
		close(born[1]);
		char c;
		while (read(born[0], &c, 1) < 0 && errno == EINTR) {
		}
		close(born[0]);
		close(confirm[0]);
		if (confirm[1] == FD_CONFIRM) {
			// dup2 onto the same descriptor does not touch FD_CLOEXEC.
			fcntl(FD_CONFIRM, F_SETFD, 0);
		} else if (dup2(confirm[1], FD_CONFIRM) < 0) {
			report("E dup confirm: %s", errno_name(errno));
			_exit(1);
		} else {
			close(confirm[1]);
		}
		return stage_b();
	}
	close(born[0]);
	// Parent: drop the child-only ends. Only then does stage B's death turn
	// into EOF on the confirm pipe.
	close(confirm[1]);
	close(FD_CGROUP);
	report("P %d", child);
	// Release stage B: its reports follow "P <pid>".
	if (write(born[1], "p", 1) < 0) {
		// The child is gone or the pipe broke; the confirm read below reports
		// the failure.
	}
	close(born[1]);

	char buf[128];
	ssize_t n;
	do {
		n = read(confirm[0], buf, sizeof buf - 1);
	} while (n < 0 && errno == EINTR);
	if (n > 0) {
		buf[n] = 0;
		int ws = 0;
		waitpid(child, &ws, 0);
		int code = WIFEXITED(ws) ? WEXITSTATUS(ws) : 127;
		report("X %s", buf);
		// Remove the cgroup before the exit report: a restart policy may start
		// the next run as soon as the agent sees "D", and that run recreates
		// the cgroup — removing it afterwards would race the new run.
		remove_cgroup();
		report("D %d", code);
		return code;
	}
	close(confirm[0]);
	report("S %d", child);

	int ws = 0;
	if (waitpid(child, &ws, 0) != child)
		return 137;
	int code = WIFEXITED(ws) ? WEXITSTATUS(ws) : 128 + WTERMSIG(ws);
	// Same ordering as above: no live cgroup when "D" can trigger a restart.
	remove_cgroup();
	report("D %d", code);
	return code;
}

// --- capability probe ------------------------------------------------------

// probe exercises exactly the syscall classes the real path depends on:
// clone3 + CLONE_INTO_CGROUP, a new pid namespace, a private mount namespace,
// tmpfs + proc mounts, pivot_root, sethostname, no-new-privs and a seccomp
// filter. Everything happens in the child's private namespaces; only the
// scratch cgroup and the tmpfs mountpoint are shared, and both are removed.
static int probe(void)
{
	char cg[64], mnt[64];
	snprintf(cg, sizeof cg, "/sys/fs/cgroup/anvil-probe-%d", (int)getpid());
	snprintf(mnt, sizeof mnt, "/tmp/anvil-probe-%d", (int)getpid());
	int err = 0;

	if (mkdir(cg, 0755) != 0) {
		fprintf(stderr, "probe: mkdir %s: %s\n", cg, strerror(errno));
		return 1;
	}
	int cgfd = open(cg, O_RDONLY | O_DIRECTORY | O_CLOEXEC);
	if (cgfd < 0) {
		fprintf(stderr, "probe: open cgroup: %s\n", strerror(errno));
		rmdir(cg);
		return 1;
	}
	if (mkdir(mnt, 0755) != 0 && errno != EEXIST) {
		fprintf(stderr, "probe: mkdir %s: %s\n", mnt, strerror(errno));
		close(cgfd);
		rmdir(cg);
		return 1;
	}

	int used_clone3 = 0;
	pid_t child = clone_stage_b(cgfd, &err, &used_clone3);
	if (child < 0) {
		fprintf(stderr, "probe: clone3: %s\n", strerror(err));
		close(cgfd);
		rmdir(mnt);
		rmdir(cg);
		return 1;
	}
	if (child == 0) {
		if (!used_clone3)
			_exit(10);
		if (syscall(SYS_unshare, CLONE_NEWNS | CLONE_NEWUTS) != 0)
			_exit(11);
		if (mount("tmpfs", mnt, "tmpfs", 0, "size=1m") != 0)
			_exit(12);
		char old[80];
		snprintf(old, sizeof old, "%s/.pivot_root", mnt);
		if (mkdir(old, 0700) != 0)
			_exit(13);
		if (syscall(SYS_pivot_root, mnt, old) != 0)
			_exit(14);
		if (chdir("/") != 0)
			_exit(15);
		if (umount2("/.pivot_root", MNT_DETACH) != 0)
			_exit(16);
		if (mkdir("/proc", 0555) != 0 && errno != EEXIST)
			_exit(17);
		if (mount("proc", "/proc", "proc", 0, "") != 0)
			_exit(18);
		if (sethostname("anvil-probe", 11) != 0)
			_exit(19);
		if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0)
			_exit(20);
		struct anvil_sock_filter allow = {.code = 0x06, .jt = 0, .jf = 0, .k = SECCOMP_RET_ALLOW};
		struct anvil_sock_fprog prog = {.len = 1, .filter = &allow};
		if (syscall(SYS_seccomp, SECCOMP_SET_MODE_FILTER, 0, &prog) != 0)
			_exit(21);
		_exit(0);
	}

	int ws = 0;
	if (waitpid(child, &ws, 0) != child || !WIFEXITED(ws) || WEXITSTATUS(ws) != 0) {
		int code = WIFEXITED(ws) ? WEXITSTATUS(ws) : -1;
		fprintf(stderr, "probe: child failed (step %d)\n", code);
		err = 1;
	}
	close(cgfd);
	rmdir(cg);
	rmdir(mnt);
	return err;
}

// --- entry -----------------------------------------------------------------

int main(int argc, char **argv)
{
	if (argc >= 2 && strcmp(argv[1], "probe") == 0)
		return probe();
	const char *plan_path = NULL;
	int have_cgroup = 0;
	for (int i = 1; i < argc; i++) {
		if (strcmp(argv[i], "-plan") == 0 && i + 1 < argc)
			plan_path = argv[++i];
		else if (strcmp(argv[i], "-id") == 0 && i + 1 < argc)
			g_id = argv[++i];
		else if (strcmp(argv[i], "-cgroup") == 0)
			have_cgroup = 1;
	}
	if (!plan_path) {
		fprintf(stderr, "usage: %s init -plan <file> [-cgroup] | probe\n", argv[0]);
		return 2;
	}
	return run_init(plan_path, have_cgroup);
}
