# FTW rebrand and Sourceful migration

- **Status:** In implementation; publishing and external cutover gates remain manual
- **Current repository:** `frahlg/forty-two-watts`
- **Canonical repository after cutover:** `srcfl/ftw`
- **Website repository:** `frahlg/forty-two-watts-web` → `srcfl/ftw-web`
- **Canonical project name:** **FTW**
- **License:** Apache License 2.0
- **Steward:** Sourceful Labs AB and project contributors

This specification replaces a single large rename with two independently
releasable programmes:

1. **Programme A — project, code and distribution migration.** This covers the
   FTW name, repository, binaries, images, installers, operating-system assets,
   documentation, website and legal metadata.
2. **Programme B — remote infrastructure and passkey migration.** Relay/TURN
   references move to the Sourceful namespace in Programme A, while provisioning
   and the WebAuthn origin/RP-ID migration remain separate, security-reviewed
   work. The rebrand must ship without changing the current WebAuthn RP ID.

The programmes may share terminology and documentation, but they must not be
combined into one production cutover.

---

## 1. Outcome

Rename the active project identity from **forty-two-watts / 42W** to **FTW**,
move stewardship and canonical distribution under Sourceful, and correct
outdated naming, ownership, URLs and license metadata.

The migration must preserve existing installations, configuration, SQLite
state, Parquet history, device identity, integration identity and remote access.
No control behaviour, sign convention, planner behaviour, driver behaviour,
API semantics or energy data may change as an incidental part of the rebrand.

The rebrand is successful when new users encounter FTW everywhere, while an
existing installation can upgrade and continue operating without relocating
its data or re-enrolling access credentials.

---

## 2. Locked product decisions

### 2.1 Name and copy

The formal project name is:

> **FTW**

FTW has no official expansion. Do not introduce a backronym in product copy,
metadata or legal files. The former name may appear in migration guidance,
compatibility code, stable external identifiers and historical material.

Use this short descriptor:

> **Local-first home energy coordination.**

Use this long description:

> **FTW is an open-source local energy runtime for solar, batteries, grid and EV charging.**

Use this stewardship statement:

> **FTW is maintained by Sourceful Labs AB and project contributors.**

Where separation from Sourceful-hosted products matters, use:

> **FTW is a self-hosted open-source project maintained by Sourceful Labs AB. It is not a hosted Sourceful service, and the local control path does not depend on Sourceful cloud services.**

### 2.2 Usage rules

- Prose and UI: `FTW`
- Commands, binaries and new deployment assets: `ftw`
- Environment variables: keep `FTW_*`
- Formal name: never `Sourceful FTW`
- Optional marketing endorsement: `FTW by Sourceful`
- Historical and compatibility references may retain the former name when
  changing them would obscure history or break users

### 2.3 Visual identity

Create a text-first FTW wordmark that keeps the current dark, technical and
slightly playful character. Replace active `42` and `42W` marks in the UI,
favicon and social assets. Do not add a new standalone symbol as part of this
project. Logo alt text is `FTW`.

The Sourceful logo may be shown as a maintainer endorsement, not as part of the
FTW project mark.

---

## 3. Compatibility model

A repository-wide string replacement is forbidden. Every legacy reference must
be assigned to one of these classes before it is changed.

### Class 1 — presentation identity: rename now

Examples:

- UI titles, headings, onboarding and diagnostics copy
- README and active operator documentation
- startup banners and human-readable version output
- OCI title/description labels
- Raspberry Pi Imager display name and artwork
- website metadata and visible copy

These surfaces become FTW in the transition release.

### Class 2 — canonical distribution identity: add, then cut over

Examples:

- `srcfl/ftw`
- `github.com/srcfl/ftw/go`
- `ftw` binary and release archives
- `ghcr.io/srcfl/ftw`
- `~/ftw` for fresh installations
- `ftw.local` for fresh images

The new identifier must be proven before it becomes the default. The old
identifier remains available during the compatibility window.

### Class 3 — compatibility alias: retain for a defined window

Examples:

- legacy binary and release-asset names
- legacy GHCR image names
- old repository and installer URLs
- an existing `~/forty-two-watts` deployment directory
- existing Compose project, service and container names
- existing hostname `42w.local`

Support these for at least two published releases and at least 90 days, using
the longer period. The clock starts with the first canonical release from
`srcfl/ftw`; record its version and date in `MIGRATION.md`. Removing any
alias requires a separate release decision and evidence that supported upgrade
paths no longer depend on it.

### Class 4 — stable integration or wire identity: do not rename here

The current repository contains identifiers whose spelling is part of an
external contract. They are not product copy.

Keep them unchanged unless a separate, versioned migration is designed:

- Home Assistant MQTT topics under `forty-two-watts/...`
- Home Assistant discovery IDs such as `forty_two_watts`
- existing Home Assistant add-on slug and stored add-on data
- Nova gateway/hardware identifiers already containing `f42w`
- persisted config keys, SQLite table/column names and device IDs
- protocol schema names and driver-facing identifiers
- existing site IDs and owner/wallet handles
- existing `FTW_*` variables, `ftw_owner` cookies and `ftw_*` browser keys

Human-readable Home Assistant names may become FTW while entity identity,
topics and unique IDs stay stable. Otherwise users would receive duplicate
entities and broken automations after an apparently cosmetic update.

### Class 5 — security identity: split migration

Examples:

- `home.fortytwowatts.com` as WebAuthn RP ID
- allowed WebAuthn origins
- owner-access host routing
- origin-bound IndexedDB, localStorage and cookies
- owner-access host routing and origin validation

The WebAuthn RP ID and home-origin contracts remain unchanged during Programme
A. Repository defaults and documentation move relay/TURN to
`relay.ftw.sourceful.energy` and `turn.ftw.sourceful.energy` now; provisioning
and operational validation are explicitly allowed to follow later.

### Class 6 — historical or third-party reference: preserve

Preserve prior changelog entries, archived design documents, license history,
third-party license references and unrelated strings. For example, an accurate
reference to an MIT-licensed dependency is not an FTW license error.

---

## 4. Canonical targets

| Surface | Canonical target | Migration rule |
|---|---|---|
| Project name | `FTW` | Transition release |
| Repository | `srcfl/ftw` | Manual transfer checkpoint |
| Website repository | `srcfl/ftw-web` | Separate repository and PR |
| Project website | `ftw.sourceful.energy` | Cut over after canonical site is verified |
| Home origin | `home.ftw.sourceful.energy` | Programme B; old origin and RP ID remain supported |
| Relay origin | `relay.ftw.sourceful.energy` | Repository reference cutover now; infrastructure follow-up may lag |
| TURN origin | `turn.ftw.sourceful.energy` | Repository reference cutover now; infrastructure follow-up may lag |
| Go module | `github.com/srcfl/ftw/go` | Change with repository cutover PR |
| Main command | `go/cmd/ftw` | Keep legacy executable alias |
| Main binary | `ftw` | Legacy `forty-two-watts` alias for compatibility |
| Main GHCR image | `ghcr.io/srcfl/ftw` | Dual-publish before changing defaults |
| Updater image | `ghcr.io/srcfl/ftw-updater` | Version in lockstep with main image |
| Fresh install directory | `~/ftw` | Never auto-move an existing directory |
| Fresh image hostname | `ftw` / `ftw.local` | Never auto-rename an existing host |
| Fresh Pi image asset | `ftw-rpi4-arm64-vX.Y.Z.img.xz` | Legacy asset optional during window |
| Package metadata name | `ftw` | Only after the Changesets queue gate passes |
| Imager display name | `FTW` | Fresh image metadata only |

Compose project, service and container names are deployment identifiers, not a
brand acceptance criterion. Existing installations keep their current names.
Fresh-install names may become `ftw` only after the updater can target either
service name safely.

Keep the default Git branch named `master`.

---

## 5. External readiness gates

The code work may be prepared before these gates close, but no transfer or
public cutover may start until all applicable owners approve them.

### 5.1 Ownership and access

- Confirm `srcfl/ftw` and `srcfl/ftw-web` are available.
- Confirm the operator performing the transfer has admin rights in both the
  source account and Sourceful organization.
- Confirm Sourceful organization Actions, branch protection and package
  policies allow the existing workflows.
- Re-authorize or replace the fine-grained `CI_TOKEN` for the destination
  repository.
- Inventory repository secrets, variables, environments, webhooks, deploy keys,
  branch rules and required checks. Record an owner for re-verifying each one.

### 5.2 Container registry

- Prove that CI can push the same test manifest to both the Sourceful and legacy
  namespaces.
- Link each GHCR package to the intended repository and verify package
  visibility and Actions access.
- Use a narrowly scoped credential for the legacy namespace. Do not assume the
  destination repository's `GITHUB_TOKEN` can publish under `frahlg`.
- Record how legacy package publication will be retired.

### 5.3 Domains and operations

- Confirm Sourceful controls `ftw.sourceful.energy`,
  `home.ftw.sourceful.energy` and `relay.ftw.sourceful.energy`.
- The canonical TURN endpoint is `turn.ftw.sourceful.energy`. Repository
  references move in Programme A; provisioning and validating the replacement
  TURN deployment is an explicit Programme B operational follow-up. A temporary
  outage is accepted for this pre-adoption migration.
- Prepare DNS, TLS, CSP/CORS changes and rollback TTLs before website cutover.
- Keep control of `fortytwowatts.com` and all remote-access subdomains.
- Confirm the website repository, deployment platform and redirect owner.

### 5.4 Brand and legal

- Sourceful approves the wordmark, stewardship language, NOTICE and trademark
  policy.
- Complete any desired trademark/name clearance before treating the visual
  identity as final.

### 5.5 External maintainers

- Agree a Home Assistant add-on migration plan with its maintainer.
- Identify owners for Sourceful app/Nova labels, Discord and other community
  surfaces. These tasks must not block the safe runtime migration unless they
  distribute software or credentials.

---

## 6. Scope and non-goals

In scope:

- active brand and stewardship copy
- repository/module/command identity
- binaries, archives, images, installers and Pi image metadata
- safe compatibility logic for existing deployments
- release and Changesets integration
- documentation, website coordination and active license metadata
- a separate design and rollout for remote origins and passkeys

Out of scope:

- control algorithms, sign conventions, MPC behaviour and safety clamps
- YAML schema changes unrelated to compatibility
- aesthetic SQLite or state-key renames
- Home Assistant topic/unique-ID migration
- Nova protocol or provisioned identity migration
- history rewriting or broad archive cleanup
- default branch rename
- unrelated refactors discovered during implementation

File unrelated functional changes separately.

---

## 7. Programme A — project and distribution migration

### Phase A0 — inventory and freeze the migration contract

Create a machine-readable inventory from tracked files. Exclude `.git`, local
worktrees, generated binaries, virtual environments, vendored code and archive
content from the active-brand baseline.

Search content and paths for:

```text
forty-two-watts
forty_two_watts
forty two watts
42W / 42w
fortytwowatts.com
frahlg/forty-two-watts
ghcr.io/frahlg/forty-two-watts
active MIT license claims
```

Classify every result using section 3. Store the reviewed allowlist with a
reason, owner and optional removal date. The CI brand check must validate the
allowlist; it must not be an opaque grep exception that permits new references
anywhere in an allowed directory.

Before editing `package.json`, inspect all pending Changesets. The current
At the implementation baseline, Changesets referred to the package name
`forty-two-watts`. Migrate all remaining frontmatter atomically with the package
rename. A mixed queue makes the Version Packages workflow fail or target a
nonexistent package.

Deliverables:

- `docs/adr/0002-ftw-name-and-sourceful-stewardship.md`
- `MIGRATION.md` skeleton
- reviewed brand/compatibility inventory
- CI brand-cleanup check
- release and rollback owners

### Phase A1 — compatibility foundation in the legacy repository

Land compatibility before changing canonical paths.

#### Binary and CLI

- Build the canonical executable as `ftw`.
- Package `forty-two-watts` as a symlink, hard link or wrapper to the same
  executable on Unix.
- Package a compatibility copy or launcher on Windows.
- Keep `ftw pair` and `ftw nova-claim` working under both executable names.
- Do not print deprecation noise during service startup. Help or `--version`
  may include a concise migration note.

#### Docker and updater

The updater currently targets a hard-coded Compose service named
`forty-two-watts`. Make the main service configurable or discoverable before a
fresh compose file introduces `ftw`.

Required behaviour:

- accept both `forty-two-watts` and `ftw` service names
- target the exact compose file, override files and project already in use
- preserve the existing host bind path and Compose project name
- validate that exactly one main service maps the persistent data directory
- pull a versioned tag, not a moving `latest` tag
- snapshot state before recreation
- wait for an application health check after recreation
- roll back to the previous image digest if health does not recover
- never run two host-networked main containers simultaneously; both would
  contend for the same ports and hardware access

The upgrade algorithm is therefore an in-place recreate with verified rollback,
not blue/green replacement.

For existing installs, retain service, container and project names. For fresh
installs, use `ftw` only after the updater compatibility tests pass.

#### Installer

For the install directory:

1. Use an explicit `FTW_DIR` when supplied.
2. Otherwise use an existing `~/ftw`.
3. Otherwise reuse an existing `~/forty-two-watts`.
4. Otherwise create `~/ftw`.

Never auto-move an existing install. Before replacing a compose file, save the
current file, inspect its merged config and verify the data bind, project name
and main service. A failed preflight must stop before `docker compose up`.

The old installer URL may redirect, but the supported migration contract must
not depend only on that redirect. Keep a small compatibility installer or
release-pinned bootstrap for the compatibility window.

#### Dual image publishing

Publish identical multi-architecture manifests for every transition release:

```text
ghcr.io/srcfl/ftw
ghcr.io/srcfl/ftw-updater
ghcr.io/frahlg/forty-two-watts
ghcr.io/frahlg/forty-two-watts-updater
```

Verify tag parity and per-platform digests. The Sourceful images must be public
and pullable before any compose or installer default points at them.

#### Upgrade fixture

Add a versioned upgrade test that starts the latest legacy release, creates
recognisable config/state/history/device/owner records, upgrades in place and
verifies:

- the same data directory is mounted
- SQLite data and cold history remain present
- device and site IDs are unchanged
- owner records are unchanged
- only one main container is active
- the new version becomes healthy
- rollback restores the previous runtime without restoring data from scratch

### Phase A2 — transition release from `frahlg/forty-two-watts`

Publish one transition release from the legacy repository. It must contain:

- FTW presentation branding
- canonical `ftw` binary plus legacy executable alias
- both GHCR namespaces
- updater and installer compatibility from Phase A1
- migration documentation
- Apache-2.0 metadata corrections

Do not rename or transfer the repository until CI, a clean fresh install and the
legacy upgrade fixture all pass against the published release.

### Phase A3 — canonical code, package and asset identifiers

After the transition release is proven:

#### Go module and command

- Rename `go/cmd/forty-two-watts` to `go/cmd/ftw`.
- Change the module to `github.com/srcfl/ftw/go`.
- Update imports, build commands, path comments and tests.
- Run `go mod tidy` and reject unrelated dependency changes.
- Keep runtime configuration and state formats unchanged.

The repository exposes commands and internal packages rather than a supported
public Go library, but the module change still requires a clean full build and
all import references must move together.

#### Changesets and package metadata

- Add one user-facing rebrand changeset after the package-name queue gate in
  Phase A0 is resolved.
- Use a minor bump while the project remains pre-1.0 and compatibility aliases
  remain in place.
- Rename package metadata to `ftw` only in a tree where every pending changeset
  targets `ftw`.
- Do not edit `package.json` version or generated changelog entries manually.

#### Runtime and web UI

Rename presentation strings, page titles, diagnostics archive names and
human-readable logs. Do not rename structured metrics, API fields, HA identity,
Nova identity or stored keys simply to satisfy a text search.

#### Raspberry Pi image

For newly built images:

- rename the pi-gen stage and active `42w-*` host services to `ftw-*`
- use `/opt/ftw` for a fresh image if all first-boot paths move together
- use hostname `ftw` and URL `http://ftw.local/`
- publish `ftw-rpi4-arm64-vX.Y.Z.img.xz`
- display `FTW` and the locked descriptor in Imager metadata

An existing host keeps its hostname and host-level service names. The container
inside an existing image must remain upgradeable without reflashing. A temporary
`42w.local` mDNS alias is optional and must not block the release.

#### Native systemd deployment

First decide whether native systemd installation remains a supported deployment
mode. If yes, generate a portable unit using a stable path such as `/opt/ftw`
and implement a migration that cannot leave both units enabled. If no, archive
the current maintainer-specific unit and remove it from active deployment docs.
Do not ship `/home/fredde/...` in a canonical unit.

### Phase A4 — repository transfer runbook

This is a manual, witnessed checkpoint.

1. Freeze merges and record the current release/tag/digests.
2. Confirm the transition release and both image namespaces are healthy.
3. Transfer the repository to the Sourceful organization.
4. Rename it to `ftw`.
5. Update the local canonical remote to `srcfl/ftw`.
6. Verify old web and Git URLs redirect.
7. Do not create a repository at the old location; doing so can destroy the
   redirect.
8. Re-verify Actions permissions, `CI_TOKEN`, environments, branch rules,
   webhooks, deploy keys and required checks.
9. Re-link and verify GHCR packages and visibility.
10. Run test, changeset-check and a dry-run asset build without publishing a
    release.
11. Update repository description and topics.
12. Record results and approvers in the migration log.

GitHub redirects repository traffic after a transfer/rename, but redirects are
not a substitute for updating active URLs and do not migrate arbitrary external
services.

### Phase A5 — first canonical FTW release

Publish from `srcfl/ftw` and verify:

- canonical binaries and archives
- both current and legacy image paths
- release checksums and Imager JSON
- installer from the new raw GitHub URL
- self-update release lookup under the new repository
- transition from the last legacy release
- release announcement and codename notes

Recommended release note:

> **The project is now FTW and is maintained under Sourceful.** Canonical repositories, binaries, images and documentation use `FTW`/`ftw`. Existing configuration, stored energy data, install paths and remote-access credentials are unchanged. Legacy binary and image names remain available during the compatibility window.

### Phase A6 — website and public identity cutover

The website is a separate repository and deployment. Use a separate PR and
rollback plan.

Update visible copy, metadata, canonical URL, Open Graph/Twitter data, JSON-LD,
favicon, social images, repository/install links, license URL and stewardship
language. Remove active MIT project-license claims and drift-prone hard-coded
version/test/line/driver counts.

Preferred hero:

> **The open-source brain for your solar, battery and EV.**

Supporting copy:

> **FTW coordinates solar, batteries, grid and EV charging locally, in real time. One open-source runtime, no cloud dependency in the control path.**

After the new site is verified, redirect the old apex domain with path
preservation where possible. Do not apply the generic website redirect to
`home.fortytwowatts.com`. Relay and TURN use their new Sourceful targets and
need their own operational rollout and rollback.

---

## 8. Programme B — remote infrastructure, origins and passkeys

Programme B starts only after Programme A is stable and has its own ADR, threat
review, implementation PRs and rollout approval.

### Phase B0 — preserve the current security boundary

During all Programme A releases:

- RP ID remains `home.fortytwowatts.com`
- the old home origin, route and certificate remain live
- existing passkeys continue to authenticate there
- the home URL and RP ID used by deployed instances do not change
- relay/TURN defaults may already point at the Sourceful namespace and may be
  temporarily unavailable until the replacement infrastructure is provisioned
- no user is forced to re-enrol because of the repository or brand move

### Phase B1 — design the multi-origin state model

The current implementation caches one WebAuthn configuration and stores trusted
credentials without an RP-ID column. A dual-RP rollout therefore requires more
than adding a new allowed origin.

The design must cover:

- explicit `rp_id` storage for each credential, with existing rows migrated to
  `home.fortytwowatts.com`
- the key/foreign-key model for credentials, sessions and browser device keys
- WebAuthn challenge/session state bound to the selected RP ID
- verifier instances keyed by RP ID and allowed-origin set, not one global cache
- RP selection from trusted server routing/host configuration, never from an
  untrusted client-supplied RP ID
- APIs and UI that distinguish old-RP and new-RP credentials
- a rule that the last known-working credential cannot be deleted

Browser data is also origin-bound. Passkey Related Origin Requests do not move
cookies, localStorage, sessionStorage, IndexedDB device keys, P2P pins or the
browser-carried home directory. The migration design must specify how the new
origin safely reconstructs or re-establishes those records. A passkey-only test
is insufficient.

Create:

```text
docs/adr/0003-ftw-domain-and-passkey-migration.md
```

### Phase B2 — new origin using the old RP ID

Prepare `https://home.ftw.sourceful.energy` as a related origin while retaining
`home.fortytwowatts.com` as the RP ID.

Serve from the old RP-ID host:

```text
GET https://home.fortytwowatts.com/.well-known/webauthn
Content-Type: application/json
```

```json
{
  "origins": [
    "https://home.ftw.sourceful.energy"
  ]
}
```

The endpoint must be public, HTTPS, status 200 and exactly tested. Configure the
server verifier to expect the old RP ID and the actual new caller origin.

Do not depend on optimistic feature detection. Clients without Related Origin
Requests reject the ceremony with `SecurityError`; the UI must catch failure and
offer a clear link back to the old origin. Keep the old origin as a fully
supported path.

### Phase B3 — optional new-RP credential enrolment

After successful authentication with an existing credential:

1. Explain the domain migration and recovery implications.
2. Start a registration ceremony explicitly configured for RP ID
   `home.ftw.sourceful.energy` and the new origin.
3. Store the new credential with its RP ID.
4. Re-establish the new origin's browser-local device key and directory state.
5. Verify a complete sign-out/sign-in/P2P flow with the new credential.
6. Keep the old credential valid.
7. Never remove the last verified recovery path automatically.

Related-origin use and new-RP enrolment are different modes. A credential used
at the new origin with the old RP ID has not been migrated to the new RP ID.

### Phase B4 — observation and retirement decision

Retain the old home origin and RP-ID support for at least 12 months after the
new-RP flow is generally available. Retirement additionally requires:

- tested support across the project's supported browser matrix
- an operator recovery path
- evidence that active owners have a verified new-domain credential
- no critical dependency on old host routing or browser storage
- explicit Sourceful owner and security approval

Do not infer adoption solely from website traffic. Record privacy-conscious,
aggregate migration status or expose a local operator report.

---

## 9. Documentation and legal metadata

Create or update active files as applicable:

```text
README.md
MIGRATION.md
TRADEMARKS.md
NOTICE
CONTRIBUTING.md
AGENTS.md
CLAUDE.md
docs/operations.md
docs/development.md
docs/deploy-platforms.md
docs/rpi-image.md
docs/remote-access.md
docs/relay-deploy.md
docs/ftw-pair.md
docs/adr/0002-ftw-name-and-sourceful-stewardship.md
docs/adr/0003-ftw-domain-and-passkey-migration.md
```

Use this README opening:

```markdown
# FTW

> Local-first home energy coordination.

FTW is an open-source local energy runtime for solar, batteries, grid and
EV charging. It runs as a single Go binary on a Raspberry Pi or Linux host,
coordinates devices through Lua drivers, and keeps the core control loop local.

FTW is maintained by Sourceful Labs AB and project contributors.
```

The project license is Apache-2.0. Correct active project claims in README,
website, OCI, package, release and Imager metadata. Preserve accurate
third-party license references and prior-license history.

NOTICE heading:

```text
FTW
Copyright 2026 Sourceful Labs AB and the FTW contributors
```

Any trademark policy text must be approved by Sourceful before merge. Do not
add `®` or broadly apply `™` without a separate legal decision.

Do not rewrite historical changelog entries or archive documents merely to
remove the former name. Add a historical banner where context would otherwise
be confusing.

`MIGRATION.md` must record real release numbers and dates, not placeholders,
before the transition release ships.

---

## 10. External integrations

### Home Assistant

- Change display name, icons, descriptions, documentation and upstream links.
- Keep add-on slug, entity unique IDs, MQTT topics and stored data unchanged.
- Coordinate any repository rename with the external maintainer.
- Treat a future topic/slug migration as a separate compatibility project.

### Sourceful app and Nova

- Update human-readable labels, pairing copy, support docs and allowlists.
- Preserve provisioned hardware IDs, gateway serials and stable protocol values.
- Document retained legacy strings as wire identifiers.

### Community

Update actively maintained Discord, GitHub templates, social profiles, pinned
messages and screenshots. Historical posts and videos do not require rewriting.

---

## 11. Test and acceptance plan

All existing checks remain required:

```bash
make test
make e2e
make ci
make build-arm64
```

### Fresh install

- Linux Docker installer into an empty home directory
- Linux and macOS Compose validation
- native `ftw` start and legacy executable alias
- new image pulls from Sourceful GHCR
- Pi image metadata, Imager JSON and `ftw.local`
- FTW UI/README/metadata consistency

### Legacy upgrade

- upgrade from the latest published legacy release, not a synthetic approximation
- same config, SQLite databases, history and device IDs
- same owner records and remote-access defaults
- legacy install directory and Compose project reused
- one main container only
- health-checked success and tested rollback

### Distribution compatibility

- both binary names execute the same version
- old and new image tags resolve to equivalent manifests
- old installer and repository URLs remain usable during the window
- self-update resolves the canonical repository and image
- release archives contain the promised aliases and checksums

### Integration identity

- Home Assistant existing entities are updated in place, not duplicated
- MQTT command and state topics are unchanged
- Nova/device identity is unchanged
- config and persisted keys are byte-for-byte compatible where expected

### Remote access

Programme A gate:

- existing passkeys authenticate at `home.fortytwowatts.com`
- owner P2P flow and origin-bound browser state are unchanged

Programme B gates:

- exact related-origin response
- supported-browser use of old RP ID at new origin
- unsupported-browser fallback to old origin
- old- and new-RP credentials coexist
- origin-bound device key/directory/P2P state is restored safely
- last working credential cannot be deleted

### Brand and license

- no unclassified active former-brand copy
- no active MIT project-license claim
- all legacy references match the reviewed allowlist
- third-party and historical references remain intact

---

## 12. Delivery order and ownership

Prefer small, ordered PRs over one long-lived rebrand branch. Each PR owns its
paths and has an independently testable exit criterion.

1. **Decision and inventory PR** — ADR 0002, compatibility classes, inventory,
   CI guard and migration skeleton.
2. **Updater/installer compatibility PR** — dual service-name support, safe
   install detection, health check, rollback and legacy-upgrade fixture.
3. **Dual-publish PR** — both GHCR namespaces and manifest verification.
4. **Core rebrand PR** — module, command, binary aliases, build paths and runtime
   presentation strings.
5. **Packaging/Pi PR** — release assets, systemd decision and fresh image names.
6. **Docs/legal PR** — active repository docs, NOTICE and approved trademark
   policy. No archive rewrite.
7. **Transition release checkpoint.**
8. **Repository transfer runbook.**
9. **First canonical release checkpoint.**
10. **Website repository PR and DNS cutover.**
11. **Programme B design and implementation PRs.**
12. **Alias retirement PR**, only after compatibility criteria are met.

Do not split work solely by file extension: `Makefile`, release workflows,
Compose and the updater form one behavioural migration contract and must be
reviewed together even when different people edit them.

---

## 13. Rollback

Every phase must restore distribution or access without recreating user state.

Minimum capabilities:

- republish or retag the previous Docker manifests
- recreate the prior image against the unchanged data bind
- restore the previous binary package
- restore website routing independently from runtime distribution
- restore old home/relay DNS and certificates
- keep old RP-ID credentials valid throughout Programme B
- re-enable the previous release workflow after a failed transfer
- preserve GitHub's old repository redirect by never reusing the old location

Never use deletion, a fresh data directory or passkey reset as a rollback
mechanism.

---

## 14. Definition of done

Programme A is complete when:

- FTW is the canonical active name and Sourceful stewardship is accurate
- `srcfl/ftw`, `ftw.sourceful.energy`, the `ftw` binary and Sourceful GHCR
  images are canonical
- new installs use FTW defaults
- existing installs upgrade in place with unchanged state and integration IDs
- legacy binary/image/repository paths work for the declared compatibility
  window
- Apache-2.0 metadata is consistent
- all active former-name references are classified and allowlisted
- remote access still uses the old RP ID and existing passkeys still work
- release, upgrade and rollback tests pass

Programme B is complete only when:

- the new home origin works on the supported browser matrix
- old- and new-RP credentials coexist safely
- origin-bound browser data has a tested migration/recovery path
- the old origin has remained supported for the required observation period
- retirement criteria have explicit owner and security approval

The overall migration is complete only after both programmes meet their own
definitions of done. Programme A may be publicly complete while Programme B
remains intentionally in compatibility mode.
