# Migrating to FTW

> [!IMPORTANT]
> The repository transfer completed on 2026-07-16. The transition release,
> `v0.128.0`, is available from `srcfl/ftw` and publishes canonical FTW
> binaries and images while retaining compatibility aliases. Relay, TURN,
> passkey and old-domain infrastructure remain a separate follow-up programme.

The project is **FTW**, and its canonical stewardship and distribution are under
Sourceful. The migration preserves configuration, state, history, device
identity and owner access.

FTW is maintained by Sourceful Labs AB and project contributors.

## Rollout status

| Milestone | Release/date | Status |
|---|---|---|
| Compatibility foundation | `v0.128.0` / 2026-07-15 | Complete |
| Transition release from `frahlg/forty-two-watts` | `v0.128.0` / 2026-07-15 | Complete |
| Repository transfer to `srcfl/ftw` | 2026-07-16 | Complete |
| First post-transfer release from `srcfl/ftw` | Next release | Pending |
| Website cutover | TBD | Not started |
| New-domain passkey migration | Separate programme | Not started |
| Earliest compatibility-alias retirement | At least 90 days and two releases after the first post-transfer release | Not scheduled |

`v0.128.0` established the compatibility foundation before the repository
transfer. Per ADR 0002, the retirement clock starts only when the first release
is published from `srcfl/ftw`; that release is still pending. From then it runs
through at least two subsequent published releases and at least 90 days, using
the longer period. Compatibility aliases must not be removed merely because the
calendar date has passed.

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
| Project site | `fortytwowatts.com` | `ftw.sourceful.energy` | Old apex will redirect after cutover |
| Owner home origin/RP ID | `home.fortytwowatts.com` | Separate Programme B | Unchanged during the basic rebrand |
| Relay/TURN | legacy `fortytwowatts.com` hosts | `relay.ftw.sourceful.energy` / `turn.ftw.sourceful.energy` | Repository references move now; infrastructure validation follows in Programme B |

## Existing Docker installations

`v0.128.0` is the transition release. Existing managed installations should
use their normal stable update path; no directory, Compose project, service or
volume rename is required. Fresh installations pull from `ghcr.io/srcfl`, while
the legacy `ghcr.io/frahlg/forty-two-watts*` tags remain exact manifest mirrors
during the compatibility window.

Starting with the first updater fix after `v0.128.0`, an older Compose file that
hard-codes `forty-two-watts:<tag>` is handled automatically. The updater adds a
temporary canonical image override for update, restart, and rollback jobs,
without rewriting the host file, renaming the service, or moving the data bind.
It removes the override when the job ends; state rollback additionally pins the
exact running image so restoring data cannot also change application version.

Installations already running the `v0.128.0` updater need to refresh that
sidecar once before retrying the in-app update:

```bash
cd ~/ftw  # or the existing ~/forty-two-watts directory
docker compose pull ftw-updater
docker compose up -d --no-deps ftw-updater
```

This one-time bootstrap does not restart the main FTW service or touch its data.
For a copy-and-paste operator walkthrough, including verification and
troubleshooting, see the bilingual
[legacy upgrade guide](docs/upgrade-from-legacy.md).

The supported upgrade path:

1. honour an explicit `FTW_DIR`;
2. reuse `~/ftw` when it already exists;
3. otherwise reuse an existing `~/forty-two-watts`;
4. create `~/ftw` only for a fresh installation;
5. retain the existing Compose project, service and data-bind identity;
6. snapshot state, recreate the main service in place, verify health and roll
   back to the previous image if startup fails.

Do not manually rename the deployment directory or copy `data/` into a fresh
directory. A parallel empty bind can look like successful startup while hiding
the real configuration and state.

For a manually managed Compose deployment whose main image already uses
`${FTW_IMAGE_TAG}`, run the normal update from the existing deployment
directory:

```bash
docker compose pull
docker compose up -d
```

Do not copy state into a new directory as part of this update.

If the existing Compose file instead hard-codes an image tag such as
`forty-two-watts:<tag>`, refreshing the updater and using the in-app update is
the supported migration path. A plain `docker compose pull` cannot select a new
tag when the tag is hard-coded.

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
`https://github.com/srcfl/ftw/releases/latest/download/os_list.json`. Existing
devices keep their configured hostname, including `42w.local`, and continue
updating the container without requiring an SD-card reflash.

## Post-transfer verification

Verified on 2026-07-16:

- `frahlg/forty-two-watts` redirects to `srcfl/ftw`;
- releases, assets, issues and pull-request history are preserved under
  `srcfl/ftw`;
- `master` protection still requires the strict `go test + vet` check;
- Actions permissions, repository secrets, variables and environments survived
  the transfer;
- `v0.128.0` and `latest` are public multi-architecture images under
  `ghcr.io/srcfl`, with exact compatibility mirrors under `ghcr.io/frahlg`;
- the Sourceful GHCR package pages are public; their repository linkage still
  requires an authenticated package-settings confirmation after the transfer.

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

The basic rebrand does not change the production RP ID
`home.fortytwowatts.com`. Existing passkeys remain valid at the old home origin.

The future Sourceful-domain migration is a separate security-reviewed rollout.
It must support both domains, account for origin-bound browser data and prevent
removal of the last working credential. It will receive separate instructions
and a minimum 12-month old-domain support period after the new-RP flow ships.

## Rollback principle

Rollback restores distribution or routing while leaving user state untouched.
Never delete or recreate `config.yaml`, SQLite databases, cold history, device
identity or owner credentials to recover from a rebrand rollout.
