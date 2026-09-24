#define _GNU_SOURCE
#include <assert.h>
#include <elf.h>
#include <errno.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/auxv.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

static int (*vdso_clock)(clockid_t, struct timespec *);

static uint64_t ns(struct timespec t)
{
    return (uint64_t)t.tv_sec * 1000000000 + t.tv_nsec;
}

/* Resolve explicitly: some static libc builds deliberately bypass the vDSO. */
static void resolve_clock(void)
{
    unsigned char *base = (void *)getauxval(AT_SYSINFO_EHDR);
    assert(base);
    Elf64_Ehdr *eh = (void *)base;
    Elf64_Phdr *ph = (void *)(base + eh->e_phoff);
    Elf64_Dyn *dynamic = NULL;
    for (int i = 0; i < eh->e_phnum; i++)
        if (ph[i].p_type == PT_DYNAMIC) dynamic = (void *)(base + ph[i].p_vaddr);
    assert(dynamic);
    Elf64_Sym *symbols = NULL;
    char *strings = NULL;
    unsigned *hash = NULL;
    for (; dynamic->d_tag; dynamic++) {
        void *address = base + dynamic->d_un.d_ptr;
        if (dynamic->d_tag == DT_SYMTAB) symbols = address;
        if (dynamic->d_tag == DT_STRTAB) strings = address;
        if (dynamic->d_tag == DT_HASH) hash = address;
    }
    assert(symbols && strings && hash);
#ifdef __aarch64__
    const char *name = "__kernel_clock_gettime";
#else
    const char *name = "__vdso_clock_gettime";
#endif
    for (unsigned i = 0; i < hash[1]; i++)
        if (!strcmp(strings + symbols[i].st_name, name))
            vdso_clock = (void *)(base + symbols[i].st_value);
    assert(vdso_clock);
}

static void *check_clock(void *unused)
{
    (void)unused;
    const clockid_t clocks[] = {CLOCK_MONOTONIC, CLOCK_MONOTONIC_RAW, CLOCK_REALTIME};
    for (unsigned c = 0; c < sizeof(clocks) / sizeof(clocks[0]); c++) {
        struct timespec before, current, after;
        uint64_t previous = 0;
        for (int i = 0; i < 10000; i++) {
            assert(syscall(SYS_clock_gettime, clocks[c], &before) == 0);
            assert(vdso_clock(clocks[c], &current) == 0);
            assert(syscall(SYS_clock_gettime, clocks[c], &after) == 0);
            assert(current.tv_nsec >= 0 && current.tv_nsec < 1000000000);
            assert(ns(current) >= ns(before) && ns(current) <= ns(after));
            assert(ns(current) >= previous);
            previous = ns(current);
        }
    }
    struct timespec value;
    assert(vdso_clock(-123, &value) == -EINVAL);
    return NULL;
}

int main(void)
{
    resolve_clock();
    pthread_t threads[4];
    for (int i = 0; i < 4; i++) assert(!pthread_create(&threads[i], NULL, check_clock, NULL));
    for (int i = 0; i < 4; i++) assert(!pthread_join(threads[i], NULL));
    struct timespec start, end, value;
    assert(!clock_gettime(CLOCK_MONOTONIC, &start));
    for (int i = 0; i < 2000000; i++) assert(!vdso_clock(CLOCK_MONOTONIC, &value));
    assert(!clock_gettime(CLOCK_MONOTONIC, &end));
    printf("vDSO clock: %.2f ns/op\n", (ns(end) - ns(start)) / 2000000.0);
    puts("CLOCK PASS");
    return 0;
}
