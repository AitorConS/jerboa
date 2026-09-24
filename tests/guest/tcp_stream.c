#define _GNU_SOURCE
#include <assert.h>
#include <netinet/in.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/socket.h>
#include <unistd.h>

#define SIZE (16 * 1024 * 1024)
#define CHUNK (128 * 1024)

static void *send_stream(void *argument)
{
    int fd = *(int *)argument;
    unsigned char *buffer = malloc(SIZE);
    assert(buffer);
    for (unsigned i = 0; i < SIZE; i++) buffer[i] = i % 251;
    size_t sent = 0;
    while (sent < SIZE) {
        ssize_t n = write(fd, buffer + sent, SIZE - sent);
        assert(n > 0);
        sent += n;
    }
    assert(shutdown(fd, SHUT_WR) == 0);
    free(buffer);
    return NULL;
}

int main(void)
{
    int listener = socket(AF_INET, SOCK_STREAM, 0), client = socket(AF_INET, SOCK_STREAM, 0);
    assert(listener >= 0 && client >= 0);
    struct sockaddr_in address = {.sin_family = AF_INET, .sin_addr.s_addr = htonl(INADDR_LOOPBACK)};
    assert(bind(listener, (void *)&address, sizeof(address)) == 0);
    socklen_t length = sizeof(address);
    assert(getsockname(listener, (void *)&address, &length) == 0);
    assert(listen(listener, 1) == 0 && connect(client, (void *)&address, length) == 0);
    int peer = accept(listener, NULL, NULL);
    assert(peer >= 0);
    pthread_t writer;
    assert(pthread_create(&writer, NULL, send_stream, &client) == 0);
    unsigned char *buffer = malloc(CHUNK);
    assert(buffer);
    /* Allow a read larger than UINT16_MAX to accumulate in the receive queue. */
    usleep(100000);
    size_t received = 0;
    ssize_t n;
    while ((n = read(peer, buffer, CHUNK)) > 0) {
        for (ssize_t i = 0; i < n; i++) assert(buffer[i] == (received + i) % 251);
        received += n;
    }
    assert(n == 0 && received == SIZE);
    assert(pthread_join(writer, NULL) == 0);
    assert(close(peer) == 0 && close(client) == 0 && close(listener) == 0);
    free(buffer);
    puts("TCP STREAM PASS");
    return 0;
}
