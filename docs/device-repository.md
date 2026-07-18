# Driver repository

Lua drivers can be released independently from core. Core remains the runtime
and safety authority; downloaded code runs in the same capability-scoped Lua
sandbox as bundled drivers.

## Resolution and recovery

A configured driver resolves in this order:

1. operator-owned local override;
2. explicitly activated managed artifact;
3. bundled recovery driver.

The official signed stable repository is enabled by default when the
`device_repository` block is omitted. `enabled: false` is an explicit opt-out.
Refreshing a repository never activates code. Installation verifies and stores
one artifact, then atomically switches only that driver's active version.
SQLite records the previous artifact and every update outcome. Network or
repository failure cannot prevent Core from booting with its bundled set.

The Update Center shows the signed remote history together with every retained
local artifact. An operator can install a historical signed version or activate
an exact already-retained version without changing Core, Optimizer or another
driver.

During activation Core sends the driver's safe default mode, restarts that
driver, and waits for fresh telemetry. Success requires the same stable
hardware identity (make/serial-derived state identity) that was present before
the change. Missing telemetry, changed identity or restart failure triggers an
automatic artifact rollback. Site-meter loss inhibits dispatch while the
driver is unavailable.

## Channels

| Channel | Manifest |
|---|---|
| `beta` | `https://github.com/srcfl/ftw/releases/download/drivers-beta/manifest.json` |
| `stable` | `https://github.com/srcfl/ftw/releases/download/drivers-stable/manifest.json` |

Changes to `drivers/` on master publish beta automatically. Stable is an
explicit workflow promotion. There is no edge driver channel.

Each signed channel manifest carries prior versions in `history`. Old release
artifacts remain immutable and addressable, so a single driver can move forward
or backward independently of the current catalog head.

## Trust

`manifest.json` is an Ed25519-signed envelope. Its payload binds repository,
source commit, generation time, driver identity/version, host API range,
immutable artifact URL and SHA-256. Core verifies:

- the pinned key and signature;
- safe manifest and artifact paths;
- exact artifact hash;
- Lua lifecycle and matching `DRIVER` metadata;
- host API compatibility.

Remote Lua is never executed directly from a URL. Unsigned and insecure sources
are limited to explicit local development settings.

## Driver versions

Each driver declares public metadata in its `DRIVER` table. Executable or
public metadata changes require a higher SemVer; CI compares the worktree with
the base branch. The metadata is also the source for the in-app catalog.

```bash
cd go
go run ./cmd/ftw-driver-repository check-versions \
  -repo-root .. -base origin/master -head WORKTREE
```

Maintainer publishing and verification are implemented by
`go/cmd/ftw-driver-repository` and
`.github/workflows/drivers-release.yml`; those sources are authoritative for
flags, key configuration and artifact ordering.
