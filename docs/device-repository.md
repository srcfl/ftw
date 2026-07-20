# Driver source and signed packages

`srcfl/device-drivers` is the only editable source for shared driver code,
versions, package recipes, target adapters, contracts and tests. Private Device
Support consumes an exact public commit, builds it and signs release data. It
does not own a second editable driver tree. `drivers.sourceful.energy` is the
runtime distribution point.

FTW accepts only the exact `ftw-core` runtime profile. It never loads code from
GitHub. Core remains the activation and safety authority.

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
| `sourceful.driver-index/v1` | Signed discovery index and package envelopes from `drivers.sourceful.energy` |
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
version to `stable` without rebuilding the artifact. Package v1 signs the
channel inside its payload, so stable has a new envelope and signature. The
artifact bytes, hashes, URLs, public source commit, materials and provenance
must stay the same. There is no edge driver channel.

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

Remote Lua is never executed directly from a URL. Local unsigned drivers need
an explicit operator overlay and never claim managed or signed status.
Sourceful index/package envelopes are always signed. Cached indexes and
packages are reverified before offline discovery. A signed read-only package
also gets a host policy that denies write calls during init, poll, command,
default mode and cleanup.

The live beta path accepts read-only packages only. Packages with commands,
control capabilities, write permissions or control-enabled FTW targets stay
off. Control also needs per-driver process or heap isolation, default-mode and
lease proof, structured command results, readback, and physical HIL.

Before stable control, the trust policy must add key overlap and rotation,
emergency deny lists for keys, packages, versions and artifact hashes, and a
monotonic index policy. An offline host may keep its last verified artifact,
but an old or expired index must not offer a new install or activation.

## Driver versions

Each driver declares public runtime metadata in its `DRIVER` table. The public
repo owns the package version. Executable or public metadata changes require a
higher package SemVer. FTW refuses artifacts whose Lua id, version or
read-only value differs from the signed package.

```bash
cd go
go run ./cmd/ftw-driver-repository check-versions \
  -repo-root .. -base origin/master -head WORKTREE
```

`go/cmd/ftw-driver-repository` and `.github/workflows/drivers-release.yml` are
old migration tools, not the shared source or future publisher. New source
changes start in `srcfl/device-drivers`. Device Support releases only an exact,
reviewed public commit. A public commit pin in FTW may serve as a contract test;
it must not make a core release necessary for a driver update.
