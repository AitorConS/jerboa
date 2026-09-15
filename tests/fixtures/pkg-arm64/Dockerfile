FROM gcc:14-bookworm AS build
WORKDIR /src
COPY service.c message.c ./
RUN mkdir -p /fixture/opt/service/bin /fixture/opt/service/lib && gcc -shared -fPIC -Wl,-soname,libmessage.so -o /fixture/opt/service/lib/libmessage.so message.c && gcc -o /fixture/opt/service/bin/service service.c -L/fixture/opt/service/lib -lmessage -Wl,-rpath,'$ORIGIN/../lib'
FROM debian:bookworm-slim
COPY --from=build /fixture/ /
ENTRYPOINT ["/opt/service/bin/service"]
