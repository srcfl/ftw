# Migrating to FTW

> [!IMPORTANT]
> Implementation is in progress, but no migration release has been published.
> The currently published repository, binaries, images, installer and
> remote-access infrastructure may still use legacy identifiers. Do not change
> a production installation based on target values in this document until a
> release is listed as available below.

The project is becoming **FTW** and its canonical stewardship and distribution
will move under Sourceful. The migration is designed to preserve configuration,
state, history, device identity and owner access.

FTW is maintained by Sourceful Labs AB and project contributors.

## Rollout status

| Milestone | Release/date | Status |
|---|---|---|
| Compatibility foundation | TBD | Not released |
| Transition release from `frahlg/forty-two-watts` | TBD | Not released |
| Repository transfer to `srcfl/ftw` | TBD | Not started |
| First canonical FTW release | TBD | Not released |
| Website cutover | TBD | Not started |
| New-domain passkey migration | Separate programme | Not started |
| Earliest compatibility-alias retirement | TBD | Not scheduled |

The compatibility clock starts with the first canonical release from
`srcfl/ftw`. It runs for at least two published releases and at least 90
days, using the longer period. Exact versions and dates will be recorded here.

## Identifier map

| Surface | Current | Target | Existing-install behaviour |
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

No action is required before a transition release is announced.

The supported upgrade path will:

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

Manual Compose deployments will receive exact, release-specific instructions
here when the compatibility release is published.

## Native binaries

The canonical executable will be `ftw`. Release archives will also contain a
`forty-two-watts` compatibility launcher or alias during the compatibility
window. Service startup through the legacy name will not emit deprecation noise.

Exact archive names and installation commands will be added when the transition
release exists.

## Source checkouts

After the repository transfer is complete, update an existing clone with:

```bash
git remote set-url origin https://github.com/srcfl/ftw.git
```

Do not run this yet. The target repository is not declared live until the
transfer milestone above is marked complete.

The Go module-path change lands as one atomic code change. There is no supported
public library API in the module today, but all local imports, build scripts and
tooling still have to move together.

## Raspberry Pi installations

New images will use `ftw.local` and FTW-branded host services. Existing devices
keep their configured hostname, including `42w.local`, and continue updating
the container without requiring an SD-card reflash.

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
