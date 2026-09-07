# FTW shadow protocol v1 fixtures

These files freeze every v1 request and response frame. The acknowledgement
kind has separate commit and flush examples. Each file contains lowercase hex,
one frame, and one trailing newline. Decode the hex before passing it to a wire
codec.

`SHA256SUMS` hashes the `.hex` files as stored, including their final newline.
The Rust test checks the manifest with a small test-only SHA-256 function. Go
can use its standard `crypto/sha256` package for the same check.

Rust tests build the named message, require an exact byte match, decode the
fixture, and require the same typed value. A Go client must run the same four
checks against these files before it can claim v1 support:

1. hex decoding succeeds;
2. the frame checksum and all limits pass;
3. decoding yields the fields built in `tests/shadow_protocol_v1.rs`;
4. encoding that value yields the exact fixture bytes.

Do not replace a fixture after release. A byte change needs a new protocol
version and a new directory. The commit fixture covers all catalog record
kinds, every property tag, both rollup resolution forms, every point field,
optional values, negative power, UTC microseconds, a run, a plan, and a
hardware-style telemetry value. Existing unit tests freeze every enum tag and
cover the other enum values.

`health-response.hex` includes trailing ops fields (overload and protocol-error
counts, database bytes/points/commits, recovered tail, sync policy, and
last-ack durable). Decoders must still accept the shorter v1 prefix that omits
those fields and treat missing counts as zero with sync policy `always`.

The corpus uses big-endian wire values. Its shared IDs include:

- source: `00112233445566778899aabbccddeeff`;
- sequence: `0102030405060708`;
- commit: `ffeeddccbbaa99887766554433221100`;
- series: `1122334455667788`.

The source sequence is an opaque, strictly increasing cursor. It does not need
to rise by one. Exact retries must reuse source, sequence, commit ID, and bytes.
