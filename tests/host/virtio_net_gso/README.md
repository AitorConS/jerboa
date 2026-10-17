# Virtio TCPv4 transmit aggregation tests

Run `tests/host/virtio_net_gso/run.sh` from the repository checkout. A host C
compiler with AddressSanitizer and UndefinedBehaviorSanitizer is required;
`CC` can select it. Build products are temporary and removed on exit.

Packet and aggregate vectors verify eligibility, sequence/ID wrap, header
compatibility, checksum seeds, segment reconstruction and scalar fallback.
The lifecycle harness includes the production `virtio_net_gso.inc` directly
with mocked allocation, DMA, queues and context primitives. It injects failures
at every reservation step and checks ownership, queue accounting, nesting,
interleaved contexts, interrupt bypass, partial feature negotiation and the
60,001-byte first-frame bound. Its context interleaving is deterministic and
does not replace real guest SMP or retransmission tests.

The guest fixture in `tests/guest/tcp_integrity/` checks complete transfers on
the actual guest network. Passing host mocks alone is insufficient to validate
virtio feature negotiation or DMA behavior.
