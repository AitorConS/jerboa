#define _GNU_SOURCE
#include <assert.h>
#include <netinet/tcp.h>
#include <netinet/in.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/socket.h>
#include <unistd.h>

/* Exercise an explicit receive budget below the default TCP window. The
 * listener option must carry over to the accepted socket. */
#define SIZE (4 * 1024 * 1024)
#define CHUNK (16 * 1024)
#ifndef SEND_CHUNK
#define SEND_CHUNK CHUNK
#endif
_Static_assert(SEND_CHUNK > 0 && SEND_CHUNK <= CHUNK, "invalid SEND_CHUNK");
#ifndef TEST_TIMEOUT
#define TEST_TIMEOUT 30
#endif
#ifndef INITIAL_DELAY_US
#define INITIAL_DELAY_US 100000
#endif
#if defined(GROW_AFTER_SHRINK) && !defined(SHRINK_AFTER_CONNECT)
#error GROW_AFTER_SHRINK requires SHRINK_AFTER_CONNECT
#endif
static int client;

static void *send_stream(void *unused)
{
    (void)unused;
    fprintf(stderr, "TCP SMALL: sender started\n");
    unsigned char buffer[CHUNK];
    size_t sent = 0;
    while (sent < SIZE) {
        size_t length = SIZE - sent < SEND_CHUNK ? SIZE - sent : SEND_CHUNK;
        for (size_t i = 0; i < length; i++) buffer[i] = (sent + i) % 251;
        ssize_t n = write(client, buffer, length);
        assert(n > 0);
        sent += n;
        if (sent % (1024 * 1024) == 0)
            fprintf(stderr, "TCP SMALL: sent %zu\n", sent);
    }
    assert(shutdown(client, SHUT_WR) == 0);
    return NULL;
}

int main(void)
{
    alarm(TEST_TIMEOUT); /* A permanent refused_data stall must fail this regression. */
    int listener = socket(AF_INET, SOCK_STREAM, 0);
    client = socket(AF_INET, SOCK_STREAM, 0);
    assert(listener >= 0 && client >= 0);
    int requested = 1024; /* doubled by setsockopt, clamped to 2304 bytes */
#ifdef CLIENT_SMALL
    assert(setsockopt(client, SOL_SOCKET, SO_RCVBUF, &requested, sizeof(requested)) == 0);
#endif
#ifndef SHRINK_AFTER_CONNECT
    assert(setsockopt(listener, SOL_SOCKET, SO_RCVBUF, &requested, sizeof(requested)) == 0);
#endif
    struct sockaddr_in address = {.sin_family = AF_INET, .sin_addr.s_addr = htonl(INADDR_LOOPBACK)};
    assert(bind(listener, (void *)&address, sizeof(address)) == 0);
    socklen_t length = sizeof(address);
    assert(getsockname(listener, (void *)&address, &length) == 0);
    assert(listen(listener, 1) == 0);
#ifndef SHRINK_AFTER_CONNECT
    int listen_clamp = 0;
    socklen_t listen_clamp_length = sizeof(listen_clamp);
    assert(getsockopt(listener, IPPROTO_TCP, TCP_WINDOW_CLAMP,
                      &listen_clamp, &listen_clamp_length) == 0);
    assert(listen_clamp == 2304);
#endif
    assert(connect(client, (void *)&address, length) == 0);
#ifdef CLIENT_SMALL
    int clamp = 0;
    socklen_t clamp_length = sizeof(clamp);
    assert(getsockopt(client, IPPROTO_TCP, TCP_WINDOW_CLAMP, &clamp, &clamp_length) == 0);
    assert(clamp == 2304);
#endif
    int peer = accept(listener, NULL, NULL);
    assert(peer >= 0);
    int actual = 0;
    length = sizeof(actual);
    assert(getsockopt(peer, SOL_SOCKET, SO_RCVBUF, &actual, &length) == 0);
#ifndef SHRINK_AFTER_CONNECT
    assert(actual == 2304);
    int peer_clamp = 0;
    socklen_t peer_clamp_length = sizeof(peer_clamp);
    assert(getsockopt(peer, IPPROTO_TCP, TCP_WINDOW_CLAMP,
                      &peer_clamp, &peer_clamp_length) == 0);
    assert(peer_clamp == 2304);
#else
    assert(actual > 2304);
#ifdef EXPECT_PRE_CLAMP
    int pre_clamp = 0;
    socklen_t pre_clamp_length = sizeof(pre_clamp);
    assert(getsockopt(peer, IPPROTO_TCP, TCP_WINDOW_CLAMP,
                      &pre_clamp, &pre_clamp_length) == 0);
    assert(pre_clamp == EXPECT_PRE_CLAMP);
#endif
#endif
    fprintf(stderr, "TCP SMALL: accepted, rcvbuf=%d\n", actual);
    pthread_t writer;
    assert(pthread_create(&writer, NULL, send_stream, NULL) == 0);
    usleep(INITIAL_DELAY_US); /* let the sender fill the advertised window */
#ifdef SHRINK_AFTER_CONNECT
    /* The SYN-ACK has already advertised the old, larger receive window. */
    assert(setsockopt(peer, SOL_SOCKET, SO_RCVBUF, &requested, sizeof(requested)) == 0);
    length = sizeof(actual);
    assert(getsockopt(peer, SOL_SOCKET, SO_RCVBUF, &actual, &length) == 0);
    assert(actual == 2304);
    fprintf(stderr, "TCP SMALL: shrunk rcvbuf=%d after sender start\n", actual);
#endif
    fprintf(stderr, "TCP SMALL: reader started\n");
    unsigned char buffer[CHUNK];
    size_t received = 0;
    ssize_t n;
    unsigned reads = 0;
    while ((n = read(peer, buffer, sizeof(buffer))) > 0) {
        for (ssize_t i = 0; i < n; i++) assert(buffer[i] == (received + i) % 251);
        received += n;
        if (++reads <= 8)
            fprintf(stderr, "TCP SMALL: read %zd, total %zu\n", n, received);
#ifdef GROW_AFTER_SHRINK
        if (reads == 1) {
            int grow = 106496;
            assert(setsockopt(peer, SOL_SOCKET, SO_RCVBUF, &grow, sizeof(grow)) == 0);
            length = sizeof(actual);
            assert(getsockopt(peer, SOL_SOCKET, SO_RCVBUF, &actual, &length) == 0);
            assert(actual == 212992);
            fprintf(stderr, "TCP SMALL: regrew rcvbuf=%d after first read\n", actual);
        }
#endif
        if (received % (1024 * 1024) == 0)
            fprintf(stderr, "TCP SMALL: received %zu\n", received);
    }
    assert(n == 0 && received == SIZE);
    assert(pthread_join(writer, NULL) == 0);
    assert(close(peer) == 0 && close(client) == 0 && close(listener) == 0);
    puts("TCP SMALL RECEIVE PASS");
    return 0;
}
