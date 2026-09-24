#define _GNU_SOURCE
#include <assert.h>
#include <errno.h>
#include <fcntl.h>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <stdio.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <unistd.h>

static void *discard_pages(void *unused)
{
    (void)unused;
    size_t page = sysconf(_SC_PAGESIZE), length = page * 8;
    unsigned char *p = mmap(NULL, length, PROT_READ | PROT_WRITE,
                            MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    assert(p != MAP_FAILED);
    for (int round = 0; round < 100; round++) {
        memset(p, 0x80, length);
        assert(madvise(p + page, page * 6, MADV_DONTNEED) == 0);
        for (size_t i = 0; i < length; i++)
            assert(p[i] == ((i < page || i >= 7 * page) ? 0x80 : 0));
    }
    assert(munmap(p, length) == 0);
    return NULL;
}

static void socket_buffers(void)
{
    for (int type = SOCK_STREAM; type <= SOCK_DGRAM; type++) {
        int a = socket(AF_INET, type, 0), b = socket(AF_INET, type, 0);
        assert(a >= 0 && b >= 0);
        const int options[] = {SO_SNDBUF, SO_RCVBUF};
        for (unsigned i = 0; i < sizeof(options) / sizeof(options[0]); i++) {
            int initial, value, requested = 8192;
            socklen_t size = sizeof(value);
            assert(getsockopt(b, SOL_SOCKET, options[i], &initial, &size) == 0);
            assert(initial > 0);
            assert(setsockopt(a, SOL_SOCKET, options[i], &requested, sizeof(requested)) == 0);
            assert(getsockopt(a, SOL_SOCKET, options[i], &value, &size) == 0);
            assert(value >= requested);
            assert(getsockopt(b, SOL_SOCKET, options[i], &value, &size) == 0);
            assert(value == initial);
            requested = -1;
            /* Linux may clamp unsigned values; Jerboa rejects negative budgets. */
            if (setsockopt(a, SOL_SOCKET, options[i], &requested, sizeof(requested)) < 0)
                assert(errno == EINVAL);
        }
        assert(close(a) == 0 && close(b) == 0);
    }
    puts("PASS per-socket send and receive buffers");
}

static void zero_device(void)
{
    unsigned char b[8192];
    memset(b, 0xa5, sizeof(b));
    int fd = open("/dev/zero", O_RDWR);
    assert(fd >= 0);
    assert(read(fd, b, sizeof(b)) == sizeof(b));
    for (unsigned i = 0; i < sizeof(b); i++) assert(b[i] == 0);
    assert(write(fd, b, sizeof(b)) == sizeof(b));
    assert(close(fd) == 0);
    puts("PASS /dev/zero");
}

static void tcp_info_units(void)
{
    int server = socket(AF_INET, SOCK_STREAM, 0), client = socket(AF_INET, SOCK_STREAM, 0);
    assert(server >= 0 && client >= 0);
    struct sockaddr_in addr = {.sin_family = AF_INET, .sin_addr.s_addr = htonl(INADDR_LOOPBACK)};
    assert(bind(server, (void *)&addr, sizeof(addr)) == 0);
    socklen_t size = sizeof(addr);
    assert(getsockname(server, (void *)&addr, &size) == 0);
    assert(listen(server, 1) == 0);
    assert(connect(client, (void *)&addr, size) == 0);
    int peer = accept(server, NULL, NULL);
    assert(peer >= 0);
    struct tcp_info info;
    size = sizeof(info);
    assert(getsockopt(client, IPPROTO_TCP, TCP_INFO, &info, &size) == 0);
    assert(info.tcpi_snd_mss > 0);
    assert(info.tcpi_snd_cwnd > 0 && info.tcpi_snd_cwnd < 1024);
    assert(close(peer) == 0 && close(client) == 0 && close(server) == 0);
    puts("PASS TCP_INFO congestion window in segments");
}

int main(int argc, char **argv)
{
    if (argc > 1) {
        assert(argc == 5);
        assert(!strcmp(argv[1], "before") && !strcmp(argv[2], ""));
        assert(!strcmp(argv[3], "after") && !strcmp(argv[4], ""));
        puts("PASS empty arguments");
    }
    pthread_t workers[4];
    for (unsigned i = 0; i < 4; i++) assert(pthread_create(&workers[i], NULL, discard_pages, NULL) == 0);
    for (unsigned i = 0; i < 4; i++) assert(pthread_join(workers[i], NULL) == 0);
    puts("PASS concurrent madvise discarded pages and preserved neighbours");
    socket_buffers();
    zero_device();
    tcp_info_units();
    puts("BENCHMARK REGRESSIONS PASS");
    return 0;
}
