#define _GNU_SOURCE
#include <assert.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

/* Writes past EOF must never expose earlier contents of the blocks that the
 * file is extended over, whether they belonged to this file (freed by a
 * truncate) or to another file (freed by unlink).
 *
 *   /own:   8 MiB of 0xA5, fsync, shrink to 1 MiB + 123, fsync, write one page
 *           at 1 MiB + 64 KiB. Gap [1 MiB + 123, 1 MiB + 64 KiB) must be 0.
 *   /cross: 1 MiB of 0x11, fsync; then /victim (16 MiB of 0xA5, allocated
 *           after /cross), fsync, unlink, sync; then write one page of /cross
 *           at 1 MiB + 64 KiB. Gap [1 MiB, 1 MiB + 64 KiB) must be 0.
 *
 * argv[1] "clean": fsync and mark done; the next boot (same disk) re-reads both
 * gaps from storage. "crash": the far pages are left unsynced, a marker is
 * synced and the program waits for the host to kill the VMM; the next boot
 * checks each gap byte is 0 and each far-page byte is 0 or its written value.
 * Run with a 256 MiB disk. No expected byte is ever 0xA5. */

#define OWN_SIZE (8u << 20)
#define OWN_CUT ((1u << 20) + 123)
#define CROSS_SIZE (1u << 20)
#define FAR ((1u << 20) + 64 * 1024)
#define PAGE 4096u
#define STALE 0xa5

static unsigned char buf[16u << 20];

static void write_all(int fd, const void *b, size_t n, off_t off)
{
    const unsigned char *p = b;
    while (n) {
        ssize_t w = pwrite(fd, p, n, off);
        assert(w > 0);
        p += w; n -= (size_t)w; off += w;
    }
}

static int check(const char *path, off_t gap_start, unsigned char far_value,
                 int far_may_be_zero, const char *when)
{
    int fd = open(path, O_RDONLY);
    assert(fd >= 0);
    size_t want = FAR + PAGE - gap_start;
    ssize_t r = pread(fd, buf, want, gap_start);
    close(fd);
    /* After a crash the extension may not have been persisted: a short read
     * (old length) is valid, but every byte returned must be safe. */
    assert(r >= 0 && (r == (ssize_t)want || far_may_be_zero));
    size_t len = (size_t)r;
    for (size_t i = 0; i < len; i++) {
        off_t off = gap_start + (off_t)i;
        unsigned char got = buf[i];
        int ok = off < FAR ? got == 0 : (got == far_value || (far_may_be_zero && got == 0));
        if (!ok) {
            printf("EXTEND GAP STALE %s %s at %lld: 0x%02x%s\n", path, when, (long long)off,
                   got, got == STALE ? " (old data)" : "");
            return 1;
        }
    }
    return 0;
}

static void mark(const char *path)
{
    int d = open(path, O_CREAT | O_WRONLY, 0600);
    assert(d >= 0 && fsync(d) == 0 && close(d) == 0);
}

int main(int argc, char **argv)
{
    setbuf(stdout, NULL);
    const char *mode = argc > 1 ? argv[1] : "clean";
    int crashed = access("/gap-crash", F_OK) == 0;
    if (access("/gap-done", F_OK) == 0 || crashed) {
        const char *when = crashed ? "after crash" : "after reboot";
        if (check("/own", OWN_CUT, 0x3c, crashed, when) ||
            check("/cross", CROSS_SIZE, 0x4d, crashed, when))
            return 1;
        puts(crashed ? "EXTEND GAP CRASH PASS" : "EXTEND GAP RESTART PASS");
        return 0;
    }

    int own = open("/own", O_CREAT | O_TRUNC | O_RDWR, 0600);
    assert(own >= 0);
    memset(buf, STALE, OWN_SIZE);
    write_all(own, buf, OWN_SIZE, 0);
    assert(fsync(own) == 0 && ftruncate(own, OWN_CUT) == 0 && fsync(own) == 0);
    /* Extend /own right away, while its freed tail is the adjacent free space. */
    memset(buf, 0x3c, PAGE);
    write_all(own, buf, PAGE, FAR);

    int cross = open("/cross", O_CREAT | O_TRUNC | O_RDWR, 0600);
    assert(cross >= 0);
    memset(buf, 0x11, CROSS_SIZE);
    write_all(cross, buf, CROSS_SIZE, 0);
    assert(fsync(cross) == 0);
    int victim = open("/victim", O_CREAT | O_TRUNC | O_WRONLY, 0600);
    assert(victim >= 0);
    memset(buf, STALE, sizeof(buf));
    write_all(victim, buf, sizeof(buf), 0);
    assert(fsync(victim) == 0 && close(victim) == 0 && unlink("/victim") == 0);
    sync();

    memset(buf, 0x4d, PAGE);
    write_all(cross, buf, PAGE, FAR);
    if (check("/own", OWN_CUT, 0x3c, 0, "before reboot") ||
        check("/cross", CROSS_SIZE, 0x4d, 0, "before reboot"))
        return 1;
    if (strcmp(mode, "crash") == 0) {
        mark("/gap-crash");
        puts("EXTEND GAP CRASH POINT");
        for (;;)
            sleep(1);
    }
    assert(fsync(own) == 0 && fsync(cross) == 0 && close(own) == 0 && close(cross) == 0);
    mark("/gap-done");
    puts("EXTEND GAP PASS");
    return 0;
}
