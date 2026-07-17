# Migrating to FTW

> [!IMPORTANT]
> The repository transfer completed on 2026-07-16. The transition release,
> `v0.128.0`, and first post-transfer release, `v0.128.1`, are available from
> `srcfl/ftw`. Canonical FTW binaries and images retain compatibility aliases.
> The legacy relay, TURN, passkey portal and support tunnel were retired on
> 2026-07-17 rather than migrated to a new domain.

The project is **FTW**, and its canonical stewardship and distribution are under
Sourceful. The migration preserves configuration, state, history and device
identity.

FTW is maintained by Sourceful Labs AB and project contributors.

## Rollout status

| Milestone | Release/date | Status |
|---|---|---|
| Compatibility foundation | `v0.128.0` / 2026-07-15 | Complete |
| Transition release from `frahlg/forty-two-watts` | `v0.128.0` / 2026-07-15 | Complete |
| Repository transfer to `srcfl/ftw` | 2026-07-16 | Complete |
| First post-transfer release from `srcfl/ftw` | `v0.128.1` / 2026-07-16 | Complete |
| Website cutover | 2026-07-17 | Complete; old apex/www redirect to `ftw.sourceful.energy` |
| Legacy relay/TURN/passkey retirement | 2026-07-17 | Complete; no replacement FTW portal |
| Earliest compatibility-alias retirement | Not before 2026-10-14 and two later published releases | Not scheduled |

`v0.128.0` established the compatibility foundation before the repository
transfer. Per ADR 0002, `v0.128.1` started the retirement clock on 2026-07-16.
Compatibility aliases remain through at least two subsequent published
releases and at least 90 days, using the longer period. They must not be removed
merely because the calendar date has passed.

## Identifier map

| Surface | Legacy | Canonical | Existing-install behaviour |
|---|---|---|---|
| Repository | `frahlg/forty-two-watts` | `srcfl/ftw` | Old Git URL retained through GitHub redirect |
| Go module | `github.com/frahlg/forty-two-watts/go` | `github.com/srcfl/ftw/go` | Source builds update imports after cutover |
| Main binary | `forty-two-watts` | `ftw` | Legacy executable alias retained during compatibility |
| Main image | `ghcr.io/frahlg/forty-two-watts` | `ghcr.io/srcfl/ftw` | Legacy image tags dual-published during compatibility |
| Updater image | `ghcr.io/frahlg/forty-two-watts-updater` | `ghcr.io/srcfl/ftw-updater` | Updater remains compatible with the installed Compose layout |
| Fresh install directory | `~/forty-two-watts` | `~/ftw` | Existing directory is reused in place |
| Fresh hostname | `42w.local` | `ftw.local` | Existing hostname is not changed automatically |
| Project site | `fortytwowatts.com` | `ftw.sourceful.energy` | Old apex and `www` redirect to the canonical site |
| Owner home origin/RP ID | `home.fortytwowatts.com` | Retired | Existing passkeys are no longer used by FTW |
| Relay/TURN | legacy `fortytwowatts.com` hosts | Retired | No Sourceful replacement endpoint; old relay returns `410 Gone` |

## Existing Docker installations

`v0.128.0` is the transition release. Existing managed installations should
run the one-time migration in the bilingual
[legacy upgrade guide](docs/upgrade-from-legacy.md). The migration rewrites
only the two effective image references to `ghcr.io/srcfl`, while retaining the
directory, Compose project, main-service name, environment, volumes and exact
host data bind. It downloads both images before stopping the main service and
keeps Compose/container rollback backups.

Do not rely on refreshing only `ftw-updater` when the Compose main image is
hard-coded or still local. That cannot change the host's stale image reference
and was the source of the `does not reference FTW_IMAGE_TAG` failure.

The supported upgrade path:

1. honour an explicit `FTW_DIR`;
2. reuse `~/ftw` when it already exists;
3. otherwise reuse an existing `~/forty-two-watts`;
4. create `~/ftw` only for a fresh installation;
5. retain the existing Compose project, service and data-bind identity;
6. back up the Compose/container deployment, recreate the main service in
   place, verify the exact data bind and health, and roll back if startup fails.

Do not manually rename the deployment directory or copy `data/` into a fresh
directory. A parallel empty bind can look like successful startup while hiding
the real configuration and state.

For a manually managed Compose deployment whose main image already uses
`ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}` and whose updater is also under
`ghcr.io/srcfl`, run the normal update from the existing deployment directory:

```bash
docker compose pull
docker compose up -d
```

Do not copy state into a new directory as part of this update.

If the existing Compose file instead hard-codes an image tag such as
`forty-two-watts:<tag>`, use the legacy migration guide. A plain
`docker compose pull` cannot select a new tag when the tag is hard-coded.

## Native binaries

The canonical executable is `ftw`. The `v0.128.0` Linux and Windows archives
also contain `forty-two-watts` / `forty-two-watts.exe` compatibility binaries.
Canonical `ftw-*` and legacy `forty-two-watts-*` release assets have identical
payload digests and remain available during the compatibility window. Service
startup through the legacy name does not emit deprecation noise.

## Source checkouts

Update an existing clone with:

```bash
git remote set-url origin https://github.com/srcfl/ftw.git
```

The old GitHub and Git clone URLs redirect to `srcfl/ftw`, but updating the
remote makes the canonical ownership explicit and avoids relying on redirects.

The Go module-path change landed as one atomic code change. There is no supported
public library API in the module today, but all local imports, build scripts and
tooling still have to move together.

## Raspberry Pi installations

The `v0.128.0` SD-card image uses `ftw.local` and FTW-branded host services.
Raspberry Pi Imager metadata is published at
`https://github.com/srcfl/ftw/releases/download/rpi-installer/os_list.json`.
The installer image refreshes independently from application releases; existing
devices keep their configured hostname, including `42w.local`, and continue
updating the container without requiring an SD-card reflash.

## Post-transfer verification

Verified on 2026-07-16:

- `frahlg/forty-two-watts` redirects to `srcfl/ftw`;
- releases, assets, issues and pull-request history are preserved under
  `srcfl/ftw`;
- `master` protection still requires the strict `go test + vet` check;
- Actions permissions, repository secrets, variables and environments survived
  the transfer. The stored `CI_TOKEN` did not retain pull-request access to the
  transferred repository and must be re-authorized or replaced; the
  `v0.128.1` Version Packages PR was opened manually from the branch that
  Changesets generated;
- `v0.128.0`, `v0.128.1` and `latest` are public multi-architecture images
  under `ghcr.io/srcfl`, with exact compatibility mirrors under
  `ghcr.io/frahlg`;
- the one-command legacy migration, the `v0.128.0` to `v0.128.1` in-app update,
  and a post-update sidecar restart were verified end to end on an ARM64
  Raspberry Pi while retaining the original Compose project and data bind;
- the Sourceful GHCR package pages are public, and their repository linkage was
  confirmed after the transfer.

GitHub currently redirects the old repository, PR and release URLs. Some
numbered issue URLs under the old namespace returned 404 immediately after the
transfer even though their canonical `srcfl/ftw/issues/<number>` pages and API
records exist. This is being treated as transfer propagation/support follow-up;
do not recreate a repository at the old path, because that would disable the
repository redirect.

## Home Assistant and Nova

The rebrand does not rename stable integration identities:

- existing Home Assistant MQTT topics and discovery unique IDs remain valid;
- the Home Assistant add-on slug and stored data remain valid;
- existing Nova gateway, hardware and device identifiers remain valid.

Human-readable display names and documentation may change to FTW.

## Remote access and passkeys

The old FTW relay, TURN service, WebRTC route, browser owner portal, local
passkeys and `ftw-pair` support tunnel were retired instead of migrated. FTW's
dashboard and API are LAN-local. Use an owner-operated VPN for community
full-UI access; do not port-forward the FTW API.

Existing SQLite tables from the retired owner-access implementation are left
untouched during upgrades so this cleanup does not destructively rewrite user
state, but current FTW builds neither read nor populate them. Obsolete
`remote_access` and `fleet_statistics` YAML keys are ignored and may be removed
from operator configuration.

The planned official remote experience is the optional Sourceful Energy app
over outbound-only Novacore federation. It has a separate consent and security
model; see [the decision](docs/sourceful-remote-access-v2.md).

## Rollback principle

Rollback restores distribution or routing while leaving user state untouched.
Never delete or recreate `config.yaml`, SQLite databases, cold history or
device identity to recover from a rebrand rollout.
