/* The application must not observe /data on the root filesystem while a
 * secondary disk's initial probe is pending. No sleep/retry: test first use. */
#include <assert.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/statvfs.h>
#include <unistd.h>

int main(void)
{
    struct statvfs root, data;
    assert(statvfs("/", &root) == 0);
    assert(statvfs("/data", &data) == 0);
    unsigned long long root_size = (unsigned long long)root.f_blocks * root.f_frsize;
    unsigned long long data_size = (unsigned long long)data.f_blocks * data.f_frsize;
    printf("root=%llu data=%llu\n", root_size, data_size);
    fflush(stdout);
    assert(data_size > root_size);
    const char marker[] = "separate volume ready before application startup";
    char actual[sizeof(marker)];
    int fd = open("/data/mount-marker", O_RDWR | O_CREAT, 0600);
    assert(fd >= 0);
    assert(pwrite(fd, marker, sizeof(marker), 0) == sizeof(marker));
    assert(fsync(fd) == 0);
    assert(pread(fd, actual, sizeof(actual), 0) == sizeof(actual));
    assert(memcmp(marker, actual, sizeof(marker)) == 0);
    assert(close(fd) == 0);
    puts("VOLUME MOUNT PASS");
    return 0;
}
