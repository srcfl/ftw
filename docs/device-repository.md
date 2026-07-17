# Driver repository

Lua drivers can be released independently from core. Core remains the runtime
and safety authority; downloaded code runs in the same capability-scoped Lua
sandbox as bundled drivers.

## Resolution and recovery

A configured driver resolves in this order:

1. operator-owned local override;
2. explicitly activated managed artifact;
3. bundled recovery driver.

Refreshing a repository never activates code. Installation verifies and stores
the artifact, then atomically switches the active version. SQLite records the
previous artifact for rollback. Network or repository failure cannot prevent
core from booting with its bundled set.

## Channels

| Channel | Manifest |
|---|---|
| `beta` | `https://github.com/srcfl/ftw/releases/download/drivers-beta/manifest.json` |
| `stable` | `https://github.com/srcfl/ftw/releases/download/drivers-stable/manifest.json` |

Changes to `drivers/` on master publish beta automatically. Stable is an
explicit workflow promotion. There is no edge driver channel.

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
