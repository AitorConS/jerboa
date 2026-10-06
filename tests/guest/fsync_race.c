#define _GNU_SOURCE
#include <assert.h>
#include <fcntl.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

/* Concurrent fsync must not complete before its own metadata is written.
 *
 * First boot: THREADS threads run ROUNDS rounds; in each, every thread waits
 * a different short delay, creates its own file, writes one tagged block and
 * fsyncs the file and the directory, so creations often land while another
 * thread's log write is in flight. All fsyncs must succeed. After each round
 * thread 0 overwrites a tagged progress block on the /data volume and fsyncs
 * it. tests/guest/fsync_race_check.py uses the VMM write journal: the root
 * disk rebuilt from the writes issued before each progress block must already
 * hold every file of that round. The guest then prints RACE CRASH POINT and
 * stays idle until the host kills the VMM; a reboot on the same disk checks
 * that every acknowledged file exists and is intact. */

#define THREADS 4
#define ROUNDS 200
#define BLOCK 4096

static pthread_barrier_t barrier;
static int dirfd_r, progress_fd = -1;

static void fill(char *b, int t, int r)
{
    memset(b, 'a' + t, BLOCK);
    snprintf(b, 32, "RACE-%d-%05d", t, r);
}

static void *worker(void *arg)
{
    int t = (int)(long)arg;
    char name[32], buf[BLOCK];
    for (int r = 0; r < ROUNDS; r++) {
        snprintf(name, sizeof(name), "%d-%05d", t, r);
        fill(buf, t, r);
        pthread_barrier_wait(&barrier);
        usleep((unsigned)((t * 97 + r * 31) % 400));
        int fd = openat(dirfd_r, name, O_CREAT | O_EXCL | O_WRONLY, 0644);
        assert(fd >= 0);
        assert(write(fd, buf, BLOCK) == BLOCK);
        assert(fsync(fd) == 0);
        assert(close(fd) == 0);
        assert(fsync(dirfd_r) == 0);
        pthread_barrier_wait(&barrier);
        if (t == 0 && progress_fd >= 0) {
            memset(buf, 'p', BLOCK);
            snprintf(buf, 32, "PROGRESS-%05d", r);
            assert(pwrite(progress_fd, buf, BLOCK, 0) == BLOCK && fsync(progress_fd) == 0);
        }
    }
    return 0;
}

int main(void)
{
    setvbuf(stdout, NULL, _IOLBF, 0);
    if (access("/r/started", F_OK) != 0) {
        assert(mkdir("/r", 0755) == 0);
        int fd = open("/r/started", O_CREAT | O_WRONLY, 0644);
        assert(fd >= 0 && fsync(fd) == 0);
        close(fd);
        dirfd_r = open("/r", O_RDONLY | O_DIRECTORY);
        assert(dirfd_r >= 0 && fsync(dirfd_r) == 0);
        progress_fd = open("/data/progress", O_CREAT | O_WRONLY, 0644);
        pthread_t th[THREADS];
        assert(pthread_barrier_init(&barrier, NULL, THREADS) == 0);
        for (long t = 0; t < THREADS; t++)
            assert(pthread_create(&th[t], NULL, worker, (void *)t) == 0);
        for (int t = 0; t < THREADS; t++)
            pthread_join(th[t], NULL);
        printf("RACE CRASH POINT\n");
        for (;;)
            pause();
    }
    int missing = 0, bad = 0;
    char name[32], want[BLOCK], got[BLOCK];
    for (int t = 0; t < THREADS; t++)
        for (int r = 0; r < ROUNDS; r++) {
            snprintf(name, sizeof(name), "/r/%d-%05d", t, r);
            int fd = open(name, O_RDONLY);
            if (fd < 0) {
                if (missing++ < 8)
                    printf("RACE MISSING %s\n", name);
                continue;
            }
            fill(want, t, r);
            if (read(fd, got, BLOCK) != BLOCK || memcmp(got, want, BLOCK)) {
                if (bad++ < 8)
                    printf("RACE BAD %s\n", name);
            }
            close(fd);
        }
    printf("RACE missing %d bad %d\n", missing, bad);
    printf(missing || bad ? "RACE VERIFY FAIL\n" : "RACE VERIFY PASS\n");
    return missing || bad;
}
