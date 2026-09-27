# Concurrent guest TCP integrity

Build the static fixture for each host architecture:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/tcp-integrity ./tests/guest/tcp_integrity
```

Use `GOARCH=arm64` for the macOS native guest. Package the resulting binary
with `jerboa build /tmp/tcp-integrity --name tcp-integrity --port 39123`.
Run the two guests on the same isolated test network. Set the server address
explicitly and supply its assigned IP to the client. For example, their
respective `jerboa run -e` values are:

- `GSO_ARGS=-listen,-addr,0.0.0.0:39123,-workers,4`
- `GSO_ARGS=-addr,10.100.0.210:39123,-workers,4,-bytes,33554432`

The comma delimiter avoids whitespace in boot-argument environment values.
Normal command-line flags also work when running directly on the host.
Add `-initial-pause,500ms,-read-delay,1ms,-receive-buffer,4096` to the server
for delayed reads and a shrunken receive window. Repeat with multiple vCPUs.

Require exit zero and both exact markers:
`TCP payload SHA-256 checks passed` and
`host received all TCP payloads with matching SHA-256`.
The receiver independently generates expected bytes. The sender verifies the
returned SHA-256 and acknowledges delivery before half-close; the receiver
requires that acknowledgement and EOF. A one-second server teardown grace
lets final FIN packets leave before the single-process VM terminates.
These are correctness checks, not scored throughput benchmarks.
