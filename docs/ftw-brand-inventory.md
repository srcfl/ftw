# FTW brand and compatibility inventory

- **Implementation baseline:** `origin/master` at `6638f1ac` (2026-07-15),
  retained as the historical comparison point for the migration
- **Decision source:** [ADR 0002](adr/0002-ftw-name-and-sourceful-stewardship.md)
- **Implementation source:** [FTW rebrand and Sourceful migration](superpowers/specs/2026-07-13-ftw-rebrand-and-sourceful-migration.md)

This inventory routes legacy identifiers to the correct migration behaviour.
It is not permission for a global replacement.

The CI check compares exact active product-copy lines with the pull request's
base revision (or the previous revision on a direct push). Removing a legacy
line is always allowed. Introducing a new line containing active legacy product
copy fails CI until it is changed to FTW or explicitly reviewed.

## Search scope

The inventory covers tracked source, configuration, packaging, deployment,
active documentation and UI files. Generated binaries, virtual environments,
vendored frontend code and local worktrees are excluded.

Historical and migration-specific files are classified by path and excluded
from the active-copy baseline:

- `CHANGELOG.md`
- `docs/archive/**`
- `docs/blog/**`
- `.changeset/**`
- `MIGRATION.md`
- ADR 0002, this inventory and the rebrand implementation specification

Those exclusions do not make old identifiers canonical. They preserve history
or explain compatibility.

## Classification

| Class | Behaviour | Concrete repository surfaces | Exit condition |
|---|---|---|---|
| 1. Presentation | Rename to FTW | `README.md`, active `docs/**`, `web/**` titles/copy, setup wizard, diagnostics names, log/version text, driver author labels | Transition release |
| 2. Canonical distribution | Add and prove new target before default cutover | `go/go.mod` and imports, `go/cmd/ftw`, `package.json`, Makefile, Dockerfiles, workflows, installers, self-update repository lookup | First canonical release |
| 3. Compatibility alias | Retain old identifier temporarily | binary/archive aliases, old GHCR tags, old installer URL, existing install directory, Compose project/service/container, systemd migration, `42w.local` | At least two releases and 90 days after canonical release |
| 4. Stable integration/wire ID | Do not rename in Programme A | HA topics/discovery IDs/client identity, existing add-on slug/data, Nova `f42w` identities, persisted schema/keys, API/metric identifiers | Separate versioned migration, if ever |
| 5. Security identity | Split handling | Keep the current WebAuthn RP ID/origins until a dedicated passkey migration; move relay/TURN references to the Sourceful namespace now | Programme B provisions and validates the new remote-access infrastructure |
| 6. Historical/third-party | Preserve | changelog, archives, prior-license history, accurate dependency license references, unrelated third-party strings | No cleanup required |

## Implemented migration ledger

### Distribution and build

- `go/go.mod` and Go imports use `github.com/srcfl/ftw/go`.
- The main command lives at `go/cmd/ftw`.
- `Makefile`, `Dockerfile`, release workflows and `scripts/release.sh` build and
  package `ftw`; transition artifacts also carry the legacy executable/archive
  aliases.
- `package.json` and every pending Changeset target package `ftw` atomically.
- Release asset and self-update workflows depend on the repository/image name.
- Workflows publish canonical `srcfl` images and transition aliases. Public
  canonical and legacy manifests were verified after the transfer; authenticated
  package-to-repository linkage remains an administrative follow-up.

### Docker and installers

- Root Linux/macOS Compose files use `ftw` for fresh installs.
- `go/cmd/ftw-updater` discovers exactly one canonical or legacy main service,
  validates its persistent data mount, waits for health and can recreate the
  previously running image on failure.
- Installers prefer an explicit directory, then existing canonical/legacy
  directories, and back up and retain an existing validated Compose layout.
- `scripts/enable-modular-stack.sh` detects the canonical `ftw` service or the
  legacy `forty-two-watts` service when adding the optimizer override to an
  existing Compose installation. The former name is a service-identity
  compatibility alias only; retire it with the other Compose aliases after at
  least two canonical releases and 90 days.
- The main container uses host networking, so two main containers cannot be
  treated as a normal blue/green deployment.
- The canonical Docker image exposes an application health check.

### Raspberry Pi and operating system

- Fresh images use `deploy/pi-gen/stage-ftw`, `ftw-*` host services,
  `/opt/ftw`, the `ftw` hostname and `ftw.local`.
- `deploy/ftw.service` uses portable `/opt/ftw`, `/etc/ftw` and `/var/lib/ftw`
  paths and provides a legacy systemd alias.
- Existing images must remain container-upgradeable without reflashing or
  hostname changes.

### Stable integration identities

- `go/internal/ha/bridge.go` publishes `forty-two-watts/...` MQTT topics and
  `forty_two_watts` discovery identity. These are compatibility contracts, not
  presentation copy.
- Home Assistant display names may change, but topics, unique IDs and add-on
  storage identity remain stable.
- `go/internal/nova/**` contains existing `f42w` examples and provisioned
  identity semantics that must not be rewritten cosmetically.
- SQLite names, device IDs, site IDs and config keys are outside the rebrand.

### Remote access

- `home.fortytwowatts.com` is the accepted WebAuthn RP ID in ADR 0001 and code.
- `go/internal/api/api_owner_access.go` currently caches one WebAuthn
  configuration and trusted credential rows do not store RP ID.
- `web/owner-access/**`, `web/p2p.js` and the relay use origin-bound cookies,
  localStorage, sessionStorage and IndexedDB data.
- `home.fortytwowatts.com` remains the WebAuthn RP ID during Programme A.
- CalDAV's runtime fallbacks in `go/internal/config/config.go` retain the former
  username and collection paths for existing configs that omitted those fields.
  Fresh configs and Settings defaults use `ftw` and `/ftw/...`; changing an
  already-subscribed collection implicitly would break calendar clients.
- `docs/upgrade-from-legacy.md` and its README callout deliberately name the
  former product, directory, service, and example image so operators can
  identify affected installations and safely cross the `v0.128.0` updater
  bootstrap boundary. Keep this guide while upgrades from the transition
  release remain supported; archive it only after that supported path ends.
- `scripts/migrate-legacy-compose.sh` is the executable counterpart to that
  guide. It detects the former directory/service/image names solely to migrate
  them to canonical Sourceful images while preserving state and rollback
  identity. Retire it on the same schedule as the legacy upgrade guide.
- Relay and TURN references move to `relay.ftw.sourceful.energy` and
  `turn.ftw.sourceful.energy`; their replacement infrastructure is a Programme B
  operational follow-up, so remote access may be unavailable between the steps.

### License and history

- `LICENSE` is Apache-2.0.
- `NOTICE` uses the FTW heading and correctly records prior MIT history.
- Active project-level MIT claims must be corrected; package-lock dependency
  metadata and accurate third-party MIT references remain intact.
- `CHANGELOG.md` and `docs/archive/**` are historical and are not rewritten.

## CI rule

Run:

```bash
.github/brand/check-brand-cleanup.sh
```

The comparison check intentionally targets standalone product-copy forms such as
`forty-two-watts`, `42W` and `42-watts`. URLs, Go imports, image names, remote
domains and stable wire identifiers are governed by the classification table
and phase-specific tests instead of being hidden inside a broad text allowlist.
Operational skill copies are additionally required to stay in sync and contain
canonical command/image defaults, because stale executable instructions cannot
be safely grandfathered as prose.

When a new active legacy-copy line is genuinely required, document its class
and removal condition here before applying an explicit CI exception. Prefer FTW
copy or a named compatibility constant whenever possible.
