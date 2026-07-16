# Independent driver repository

FTW publishes its Lua drivers independently from core while keeping source,
tests, and release tooling in this monorepo. A driver update therefore does not
require a core image or OS update.

Core remains the runtime and safety authority. Downloaded drivers still execute
inside the same gopher-lua capability sandbox, use site sign convention, and
must be compatible with driver host API v1.

## Runtime resolution

The logical config path stays unchanged:

```yaml
drivers:
  - name: inverter
    lua: drivers/ferroamp.lua
```

FTW resolves that path in this order:

1. an operator-owned local driver,
2. an explicitly activated managed driver,
3. the driver bundled with the installed core release.

Missing network access, a bad remote manifest, or a failed driver update never
prevents core from booting. Refresh only updates a last-known-good catalog;
activation is a separate operator action.

## Official channels

The `drivers-release` workflow publishes moving GitHub Release channels from
this repository:

| Channel | Manifest |
|---|---|
| edge | `https://github.com/srcfl/ftw/releases/download/drivers-edge/manifest.json` |
| beta | `https://github.com/srcfl/ftw/releases/download/drivers-beta/manifest.json` |
| stable | `https://github.com/srcfl/ftw/releases/download/drivers-stable/manifest.json` |

Every Lua artifact has an immutable filename containing its version and SHA-256
prefix. The workflow uploads all artifacts before replacing `manifest.json`, so
a client cannot observe a manifest pointing to a missing object.

`master` automatically publishes edge. Beta and stable are explicit
`workflow_dispatch` promotions from a selected Git ref. This is independent of
the core stable/beta/edge image workflow.

## Trust model

`manifest.json` is an Ed25519-signed envelope. Its signed payload includes the
source commit, generation time, repository URL, and for every driver:

- public driver ID and SemVer,
- logical path and immutable HTTPS artifact URL,
- SHA-256 of the exact Lua bytes,
- compatible `host_api_min` / `host_api_max`,
- public catalog metadata.

FTW verifies the pinned signing key, payload schema, URL/path constraints,
artifact hash, Lua lifecycle, metadata identity/version, and host-API range
before activation. The runtime never executes a remote URL directly.

The official stable channel and public trust root are pinned in FTW. Explicit
opt-in can therefore be as small as:

```yaml
device_repository:
  enabled: true
```

The equivalent fully expanded configuration is:

```yaml
device_repository:
  enabled: true
  refresh_interval_h: 24
  repositories:
    - id: ftw-official
      name: FTW official drivers
      manifest_url: https://github.com/srcfl/ftw/releases/download/drivers-stable/manifest.json
      enabled: true
      trusted_keys:
        ftw-drivers-2026-01: MX+j27UBkyM099hTyJlmMLK9qlTTDUJsaK/vH12fFKc=
```

Omitting the block preserves bundled-only behavior. `allow_unsigned` is
restricted to local `file:` manifests; `allow_insecure` is for explicit local
development only.

## Maintainer release setup

Generate the key once from the `go/` directory:

```bash
go run ./cmd/ftw-driver-repository keygen \
  -private-out /secure/ftw-drivers-2026-01.private \
  -public-out /secure/ftw-drivers-2026-01.public
```

Keep the private file outside the repository. Configure:

- GitHub Actions secret `FTW_DRIVER_SIGNING_KEY` with the private-key file's
  base64 value;
- the public key in this document, `go/internal/config`, and the release
  workflow. Public trust-root changes are normal reviewed code changes.

The workflow refuses to publish if the keys are missing or do not form a pair.
It then builds and independently verifies the signature plus every artifact.

Local unsigned validation is available without production secrets:

```bash
cd go
go run ./cmd/ftw-driver-repository publish \
  -unsigned \
  -drivers ../drivers \
  -output ../dist/driver-repository \
  -base-url https://example.invalid/drivers \
  -repository https://github.com/srcfl/ftw
```

## Driver version discipline

Every driver declares explicit metadata:

```lua
DRIVER = {
  id = "ferroamp",
  version = "1.2.0",
  host_api_min = 1,
  host_api_max = 1,
  -- ...
}
```

CI requires a higher SemVer whenever executable or public metadata changes.
The only one-time exemption is adding unchanged `host_api_min` /
`host_api_max` declarations to a legacy driver. Artifact hashes still change,
so a modified driver can never masquerade as the old bytes.

Run the same check locally:

```bash
cd go
go run ./cmd/ftw-driver-repository check-versions \
  -repo-root .. -base origin/master -head WORKTREE
```

## Installation and rollback

The Settings device catalog and these endpoints use the same repository
manager:

| Endpoint | Purpose |
|---|---|
| `GET /api/device_repository/status` | Source, cache, active versions, and last refresh error. |
| `GET /api/device_repository/catalog` | Signed upstream candidates and compatibility. |
| `POST /api/device_repository/refresh` | Refresh manifests only; activates nothing. |
| `POST /api/device_repository/drivers/{id}/install` | Download, verify, atomically activate, restart the affected configured driver, and require fresh telemetry. |
| `POST /api/device_repository/drivers/{id}/rollback` | Restore the previous content-addressed artifact. |

Activation history is stored in SQLite. Crash reconciliation removes incomplete
transactions, and both the previous managed copy and bundled driver remain
available for recovery.

The full architectural decision is recorded in
[ADR 0003](adr/0003-modular-optimizer-and-drivers.md).
