FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171 AS build

ARG POSTGRES_PTHREADS_COMMIT=d5933309617388a7eb6b2e195352054edf438cb7

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       build-essential ca-certificates flex bison git libssl-dev \
    && rm -rf /var/lib/apt/lists/*

RUN git init /src \
    && git -C /src remote add origin https://github.com/postgrespro/postgresql.pthreads.git \
    && git -C /src fetch --depth 1 origin "${POSTGRES_PTHREADS_COMMIT}" \
    && git -C /src checkout --detach FETCH_HEAD

COPY nanos.patch /nanos.patch
RUN git -C /src apply /nanos.patch
COPY openssl.patch /openssl.patch
RUN git -C /src apply /openssl.patch
COPY shutdown.patch /shutdown.patch
RUN git -C /src apply /shutdown.patch

WORKDIR /src
RUN /src/configure --prefix=/usr/local/pgsql --without-readline --without-zlib --disable-nls --with-openssl \
    && (make -j4 || test -x src/bin/psql/psql) \
    && (make -C src/pl/plpgsql -j4 || test -f /src/src/pl/plpgsql/src/pl_gram.c) \
    && sed -i \
       -e 's/^YYSTYPE yylval;/session_local YYSTYPE yylval;/' \
       -e 's/^YYLTYPE yylloc/session_local YYLTYPE yylloc/' \
       /src/src/pl/plpgsql/src/pl_gram.c \
    && sed -i \
       -e 's/^extern YYSTYPE plpgsql_yylval;/extern session_local YYSTYPE plpgsql_yylval;/' \
       -e 's/^extern YYLTYPE plpgsql_yylloc;/extern session_local YYLTYPE plpgsql_yylloc;/' \
       /src/src/pl/plpgsql/src/pl_gram.h \
    && make -C src/pl/plpgsql -j4 \
    && make -C contrib/amcheck -j4 \
    && make -C src/backend install DESTDIR=/stage \
    && make -C src/backend/utils/mb/conversion_procs install DESTDIR=/stage \
    && make -C src/backend/snowball install DESTDIR=/stage \
    && make -C src/bin/initdb install DESTDIR=/stage \
    && make -C src/interfaces/libpq install DESTDIR=/stage \
    && make -C src/timezone install DESTDIR=/stage \
    && make -C src/pl/plpgsql install DESTDIR=/stage \
    && make -C contrib/amcheck install DESTDIR=/stage

# The statistics collector binds a UDP socket to getaddrinfo("localhost").
# Without /etc/hosts that lookup fails and the collector and autovacuum are
# disabled. nsswitch.conf only makes the files-first lookup order explicit.
RUN mkdir -p /stage/etc \
    && printf '%s\n' '127.0.0.1 localhost' '::1 localhost' >/stage/etc/hosts \
    && printf '%s\n' 'hosts: files dns' >/stage/etc/nsswitch.conf

# The superuser password comes from a BuildKit secret so it is neither a build
# argument nor an image layer. Only its SCRAM-SHA-256 verifier is stored in the
# cluster. Build with: docker build --secret id=pgpass,src=<file> ...
# Secret contents do not affect the build cache, so pass a fresh non-secret
# CLUSTER_NONCE whenever the password changes or a cached cluster is reused.
ARG CLUSTER_NONCE=unset
RUN --mount=type=secret,id=pgpass,required=true,uid=65534,mode=0400 \
    --mount=type=secret,id=pgcert,required=true \
    --mount=type=secret,id=pgkey,required=true \
    mkdir -p /stage/db /stage/tmp \
    && chown -R nobody:nogroup /stage \
    && su -s /bin/sh nobody -c \
       'LD_LIBRARY_PATH=/stage/usr/local/pgsql/lib /stage/usr/local/pgsql/bin/initdb -D /stage/db -U postgres --pwfile=/run/secrets/pgpass --auth-local=scram-sha-256 --auth-host=scram-sha-256 --data-checksums --no-locale --encoding=UTF8' \
    && printf "%s\n" \
       "listen_addresses = '*'" \
       "dynamic_shared_memory_type = mmap" \
       "max_parallel_workers = 0" \
       "max_parallel_workers_per_gather = 0" \
       "track_counts = on" \
       "autovacuum = on" \
       "password_encryption = 'scram-sha-256'" \
       "fsync = on" \
       "synchronous_commit = on" \
       "full_page_writes = on" \
       "wal_sync_method = fdatasync" \
       "ssl = on" \
       >>/stage/db/postgresql.conf \
    && install -m 0600 -o nobody -g nogroup /run/secrets/pgkey /stage/db/server.key \
    && install -m 0644 -o nobody -g nogroup /run/secrets/pgcert /stage/db/server.crt \
    && printf '%s\n' \
       'local all all scram-sha-256' \
       'hostssl all all 0.0.0.0/0 scram-sha-256' \
       'hostssl all all ::/0 scram-sha-256' \
       >/stage/db/pg_hba.conf \
    && ! grep -Eiq '(^|[[:space:]])(trust|password|md5)([[:space:]]|$)' /stage/db/pg_hba.conf \
    && cp --parents \
       /lib/ld-linux-aarch64.so.1 \
       /lib/aarch64-linux-gnu/libc.so.6 \
       /lib/aarch64-linux-gnu/libm.so.6 \
       /lib/aarch64-linux-gnu/libgcc_s.so.1 \
       /lib/aarch64-linux-gnu/libssl.so.3 \
       /lib/aarch64-linux-gnu/libcrypto.so.3 \
       /stage \
    && find /stage/db -type d -empty -exec touch '{}/.jerboa-keep' \;

FROM scratch
COPY --from=build /stage /
