# Device Support driver packages

`srcful-device-support` is the canonical owner of Sourceful driver source,
SemVer, package metadata and signed target artifacts. FTW is a target consumer:
it accepts only the exact `ftw-core` runtime profile and runs downloaded Lua in
the same capability-scoped sandbox as bundled drivers. Core remains the
activation and safety authority.

## Resolution and recovery

A configured driver resolves in this order:

1. operator-owned local override;
2. explicitly activated managed artifact;
3. bundled recovery driver.

Refreshing a signed Device Support index never activates code. FTW verifies
the index signature, the referenced package-envelope hash and signature, exact
host/runtime/ABI/profile compatibility, permissions, provenance, artifact size
and SHA-256. Installation then compiles and validates the Lua metadata before
atomically switching only that driver's active version. SQLite records the
previous artifact and every update outcome. Network or repository failure
cannot prevent Core from booting with its bundled set.

The current FTW-signed stable manifest remains the default during migration.
It uses `ftw.manifest/v1`. The Device Support host and production public key are
now provisioned, but the default stays unchanged until the signed stable index
has passed the SDM630 beta pilot and offline recovery test. `enabled: false`
remains the explicit operator opt-out.

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

## Repository formats

| Format | Role |
|---|---|
| `sourceful.driver-index/v1` | Canonical signed Device Support discovery index and package envelopes |
| `ftw.manifest/v1` | Transitional FTW-owned manifest retained until production cutover |

Use the beta channel only on a chosen pilot site:

```yaml
device_repository:
  enabled: true
  refresh_interval_h: 24
  repositories:
    - id: sourceful-device-support-beta
      name: Sourceful Device Support beta
      format: sourceful.driver-index/v1
      manifest_url: https://drivers.sourceful.energy/v1/channels/beta/index.envelope.json
      enabled: true
      trusted_keys:
        sourceful-drivers-2026-01: VfRKapKx1JDs+uSAM5MRhMcWLhfmY1kktrOlDrANn2o=
```

The future fleet default uses
`https://drivers.sourceful.energy/v1/channels/stable/index.envelope.json` with
the same pinned key. Change that default only after stable promotion. A catalog
refresh never installs or activates a driver.

Device Support publishes `beta` packages and promotes the exact reviewed
version and target hashes to `stable`. There is no edge driver channel.

The Device Support index may reference several immutable versions. FTW adapts
the newest compatible version into the Update Center and keeps older compatible
packages as history. A single driver can therefore move forward or backward
without changing Core or another driver.

## Trust

The Sourceful index and each package are separate Ed25519-signed envelopes.
The index binds exact package-envelope bytes; the package binds source commit,
build materials, driver identity/version, permissions, sign convention,
host/runtime compatibility and immutable target artifacts. Core verifies:

- both signatures against a pinned key and rejects duplicate JSON keys;
- the package-envelope hash named by the index;
- FTW product version, GopherLua 1.1.2, Lua 5.1 semantics,
  `gopher-lua-source-v1`, `sourceful.host/ftw-core/v1` and host API range;
- HTTPS URLs, safe paths, exact artifact size and SHA-256;
- Lua lifecycle and matching id/version/host API/`read_only` metadata.

Remote Lua is never executed directly from a URL. Unsigned and insecure sources
remain limited to the legacy local-development format. Sourceful index/package
envelopes are always signed. Cached indexes and packages are reverified before
offline discovery.

Phase 1 accepts read-only packages only. Packages with commands, control
capabilities, write permissions or control-enabled FTW targets fail closed.
That gate is removed only after default-mode invocation, bounded lease expiry,
structured command results and physical control HIL have explicit acceptance.

## Driver versions

Each driver declares public runtime metadata in its `DRIVER` table, while
Device Support owns the canonical package version. Executable or public
metadata changes require a higher package SemVer. FTW refuses artifacts whose
Lua id/version/read-only contract differs from the signed package.

```bash
cd go
go run ./cmd/ftw-driver-repository check-versions \
  -repo-root .. -base origin/master -head WORKTREE
```

`go/cmd/ftw-driver-repository` and `.github/workflows/drivers-release.yml` are
legacy migration tooling, not the future canonical publisher. They remain until
the Device Support stable endpoint, trust-root provisioning and recovery
cutover have been exercised. New shared driver releases happen in Device
Support once, not in FTW.
