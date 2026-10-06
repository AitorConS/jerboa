#define _GNU_SOURCE
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

/* Storage reuse across a crash. Driven by tests/guest/storage_reuse_crash.py,
 * which journals every write of the /data volume and replays the cuts a host
 * power loss could leave.
 *
 * First boot (no /reuse-phase on the root disk): three files a1..a3 on /data
 * are written and synced. a1 is truncated to 0, a2 unlinked and a3 truncated
 * to 1 MiB, without any sync. b is then written and synced; its blocks may
 * reuse the storage the others released. The guest prints REUSE CRASH POINT
 * and waits for the host to kill it.
 *
 * Later boots verify whatever survives on /data: every block of a1..a3 within
 * its length must hold that file's own data. A block of b means storage was
 * reused before the metadata that released it was durable (LEAK); another
 * file's block is FOREIGN, all zeros ZERO and anything else CORRUPT. ZERO
 * also appears when a file's log entries became durable before its data,
 * which only the ordering of data, flush and log prevents. b itself is not
 * checked: it was never fully acknowledged at every cut the host replays.
 *
 * Every 4 KiB block starts with a tag naming its file and block number. */

#define BLOCK 4096u
#define A_BLOCKS 1024u          /* 4 MiB */
#define A3_KEEP 256u            /* 1 MiB */
#define B_BLOCKS 4096u          /* 16 MiB */
#define PHASE "/reuse-phase"

static void fill(unsigned char *b, char file, uint32_t n)
{
    memset(b, file, BLOCK);
    snprintf((char *)b, 24, "JRUSE-%c-%08u", file, n);
}

static void write_file(const char *path, char file, uint32_t blocks)
{
    int fd = open(path, O_CREAT | O_TRUNC | O_WRONLY, 0644);
    assert(fd >= 0);
    static unsigned char chunk[64 * BLOCK];
    for (uint32_t n = 0; n < blocks; n += 64) {
        for (uint32_t i = 0; i < 64; i++)
            fill(chunk + i * BLOCK, file, n + i);
        assert(write(fd, chunk, sizeof(chunk)) == (ssize_t)sizeof(chunk));
    }
    assert(fsync(fd) == 0);
    close(fd);
}

static void sync_dir(const char *path)
{
    int fd = open(path, O_RDONLY | O_DIRECTORY);
    assert(fd >= 0);
    assert(fsync(fd) == 0);
    close(fd);
}

static void write_phase(void)
{
    write_file("/data/a1", '1', A_BLOCKS);
    write_file("/data/a2", '2', A_BLOCKS);
    write_file("/data/a3", '3', A_BLOCKS);
    sync_dir("/data");
    printf("REUSE A SYNCED\n");
    fflush(stdout);

    int fd = open("/data/a1", O_WRONLY);
    assert(fd >= 0 && ftruncate(fd, 0) == 0);
    close(fd);
    assert(unlink("/data/a2") == 0);
    fd = open("/data/a3", O_WRONLY);
    assert(fd >= 0 && ftruncate(fd, (off_t)A3_KEEP * BLOCK) == 0);
    close(fd);

    write_file("/data/b", 'B', B_BLOCKS);
    sync_dir("/data");
    printf("REUSE B SYNCED\n");

    fd = open(PHASE, O_CREAT | O_TRUNC | O_WRONLY, 0644);
    assert(fd >= 0 && write(fd, "verify\n", 7) == 7 && fsync(fd) == 0);
    close(fd);
    sync_dir("/");
    printf("REUSE CRASH POINT\n");
    fflush(stdout);
    for (;;)
        pause();
}

static int check_file(const char *path, char file, uint32_t max_blocks)
{
    int fd = open(path, O_RDONLY);
    if (fd < 0) {
        printf("REUSE %s absent\n", path);
        return 0;
    }
    struct stat st;
    assert(fstat(fd, &st) == 0);
    if (st.st_size % BLOCK || st.st_size / BLOCK > max_blocks) {
        printf("REUSE FAIL CORRUPT %s length %lld\n", path, (long long)st.st_size);
        return 1;
    }
    static unsigned char got[BLOCK], want[BLOCK];
    int bad = 0;
    for (uint32_t n = 0; n < st.st_size / BLOCK; n++) {
        assert(pread(fd, got, BLOCK, (off_t)n * BLOCK) == BLOCK);
        fill(want, file, n);
        if (!memcmp(got, want, BLOCK))
            continue;
        static const unsigned char zero[BLOCK];
        const char *kind = !memcmp(got, zero, BLOCK) ? "ZERO" :
                           !memcmp(got, "JRUSE-B-", 8) ? "LEAK" :
                           !memcmp(got, "JRUSE-", 6) ? "FOREIGN" : "CORRUPT";
        if (bad++ < 4)
            printf("REUSE FAIL %s %s block %u: %.17s\n", kind, path, n, got);
        if (strcmp(kind, "ZERO"))
            printf("REUSE KIND %s\n", kind);
        else if (bad == 1)
            printf("REUSE KIND ZERO\n");
    }
    printf("REUSE %s length %lld bad %d\n", path, (long long)st.st_size, bad);
    close(fd);
    return bad;
}

int main(void)
{
    setvbuf(stdout, NULL, _IOLBF, 0);
    if (access(PHASE, F_OK) != 0)
        write_phase();
    int bad = check_file("/data/a1", '1', A_BLOCKS) +
              check_file("/data/a2", '2', A_BLOCKS) +
              check_file("/data/a3", '3', A_BLOCKS);
    printf(bad ? "REUSE VERIFY FAIL\n" : "REUSE VERIFY PASS\n");
    return bad ? 1 : 0;
}
