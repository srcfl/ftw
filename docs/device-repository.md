# Device repository

Status: planned. This is not the current driver install path; bundled and
local Lua drivers under `drivers/` remain the active mechanism.

This document defines the first non-breaking device repository milestone
for forty-two-watts. The implementation target is a PR series, not a
single big-bang migration.

## Goal

forty-two-watts should discover Lua drivers from the public Hugin driver
repository by default:

```text
https://github.com/srcfl/hugin-drivers
```

Existing installations must keep working without config changes. A
driver listed today as `lua: drivers/ferroamp.lua` must still start when
there is no network, no repository cache, or no device repository config
at all.

Operators must be able to:

- see which configured drivers are bundled, local, or repository-managed
- see installed version/hash versus upstream version/hash
- choose when to update a driver
- use local custom drivers next to repository drivers
- add multiple compatible repositories, similar to HACS
- point any repository entry at another compatible manifest
- receive an update notification when a configured driver has a newer
  compatible repository version available

The runtime must never execute remote code directly. Repository drivers
are fetched to local storage, validated, hashed, and activated only after
an explicit operator action.

## Current State

As of `origin/master` at `b7906cb`:

- the default branch is `master`
- drivers are Lua files under `drivers/`
- Docker copies bundled drivers into `/app/drivers`
- the runtime image does not include `git`
- `-drivers` sets `config.DriversDirOverride`
- `lua: drivers/foo.lua` resolves to the bundled driver directory when
  `-drivers /app/drivers` is used
- `GET /api/drivers/catalog` scans one local directory and parses each
  driver's `DRIVER = { ... }` block

The Hugin driver repository currently exists on `main` and was seeded
from forty-two-watts, but it has no CI or manifest yet and does not
contain every driver from the latest bundled catalog.

## Non-Breaking Contract

The device repository feature must preserve these behaviours:

- `drivers[]` config entries keep using `lua:` paths.
- A missing or unreachable repository does not prevent startup.
- Bundled drivers remain a safe fallback.
- Existing driver names, `device_id` resolution, battery models, state
  keys, telemetry metric names, and Home Assistant topics are not
  renamed by this change.
- Local custom driver paths are never overwritten by repository sync.
- Repository update checks do not restart or replace a running driver
  unless the operator chooses an update action.

## Source Model

The catalog should be built from three source classes:

| Source | Purpose | Mutability |
|---|---|---|
| `bundled` | Drivers shipped with the application image or release tarball. | Replaced only on app upgrade. |
| `managed` | Repository drivers downloaded, validated, and explicitly activated by the operator. | Updated per driver by operator action. |
| `local` | Operator-owned files outside the managed cache. | Never modified by repository code. |

Configured runtime paths should resolve conservatively:

1. Absolute or non-standard relative `lua:` paths resolve as they do now.
2. `drivers/foo.lua` resolves to an explicitly activated managed copy
   when one exists for that logical path.
3. Otherwise it resolves to the bundled driver directory when
   `DriversDirOverride` is set.
4. Otherwise it resolves relative to the config file directory, as today.

This gives existing configs the same behaviour on day one while allowing
an installed repository update to take over only after activation.

Multiple repositories are first-class. The default Hugin repository is
just the built-in repository entry; operators can add more repositories
without disabling the default. Catalog merging must keep repository
identity on every upstream candidate so two repositories can publish the
same driver ID without becoming ambiguous.

When the same driver ID appears in multiple repositories, the UI must
show the source repository and let the operator choose which repository
to install from. Automatic tie-breaking is allowed only for display
ordering, never for installation.

## Repository Manifest

forty-two-watts should consume a static HTTPS manifest. It must not
depend on `git` in the runtime container.

Default manifest URL:

```text
https://raw.githubusercontent.com/srcfl/hugin-drivers/main/manifest.json
```

Proposed schema:

```json
{
  "schema_version": 1,
  "repository": "https://github.com/srcfl/hugin-drivers",
  "commit": "066de99bda3942f237ff87f11e291a5424c0c59c",
  "generated_at": "2026-05-04T00:00:00Z",
  "drivers": [
    {
      "id": "sungrow-shx",
      "path": "drivers/sungrow.lua",
      "filename": "sungrow.lua",
      "version": "1.0.0",
      "sha256": "hex-encoded-sha256",
      "url": "https://raw.githubusercontent.com/srcfl/hugin-drivers/main/drivers/sungrow.lua",
      "metadata": {
        "name": "Sungrow SH Hybrid Inverter",
        "manufacturer": "Sungrow",
        "protocols": ["modbus"],
        "capabilities": ["battery", "meter", "pv"],
        "verification_status": "production",
        "tested_models": ["SH5.0RT", "SH6.0RT", "SH8.0RT", "SH10RT"]
      }
    }
  ]
}
```

Rules:

- `schema_version` is required and must be supported by the client.
- `path` must be relative and stay under `drivers/`.
- `url` must be HTTPS unless an operator explicitly configures a local
  file source.
- `sha256` is the content hash of the Lua file at `url`.
- driver IDs are unique.
- versions use semver.
- metadata mirrors the existing `DRIVER` table parser so the UI can use
  one catalog model for bundled, managed, local, and upstream entries.

## Configuration

The first implementation should make the feature enabled by default but
safe without network access:

```yaml
device_repository:
  enabled: true
  refresh_interval_h: 24
  cache_dir: driver-repository/cache
  install_dir: driver-repository/installed
  repositories:
    - id: hugin
      name: Hugin drivers
      manifest_url: https://raw.githubusercontent.com/srcfl/hugin-drivers/main/manifest.json
      enabled: true
    - id: local-lab
      name: Lab drivers
      manifest_url: https://example.com/my-drivers/manifest.json
      enabled: false
  local_dirs:
    - local-drivers
```

Notes:

- `enabled: false` disables remote checks but does not disable bundled or
  local catalog scanning.
- if `repositories` is omitted, the default `hugin` entry is created in
  memory and existing configs remain valid.
- each `repositories[].manifest_url` can point to another compatible
  repository manifest.
- repository IDs are stable local identifiers used in state and API
  responses; changing one intentionally creates a new repository source.
- `cache_dir` and `install_dir` are relative to the persistent state
  directory unless absolute.
- `local_dirs` are catalog scan roots only. The repository manager must
  never write to them.
- If the entire block is omitted, defaults apply and existing configs
  remain valid.

## Persistent State

Repository activation state should live in SQLite, not by rewriting Lua
paths opportunistically on every refresh.

Minimum state:

```text
driver_repo_installs(
  id INTEGER PRIMARY KEY,
  repo_url TEXT NOT NULL,
  repo_id TEXT NOT NULL,
  driver_id TEXT NOT NULL,
  logical_path TEXT NOT NULL,
  version TEXT NOT NULL,
  sha256 TEXT NOT NULL,
  installed_path TEXT NOT NULL,
  previous_installed_path TEXT,
  installed_at_ms INTEGER NOT NULL,
  active INTEGER NOT NULL DEFAULT 1
)
```

This lets the resolver answer "which local file should `drivers/foo.lua`
execute?" without changing old config files. It also gives the update
endpoint enough information to roll back if validation or restart fails.

The state key for an active managed driver is `(repo_id, driver_id,
logical_path, sha256)`, not only `driver_id`. That preserves the
operator's choice when several repositories publish similarly named
drivers.

## API Surface

Extend `GET /api/drivers/catalog` without breaking existing clients.
Existing fields stay as-is; new fields are additive.

Example entry:

```json
{
  "path": "drivers/sungrow.lua",
  "filename": "sungrow.lua",
  "id": "sungrow-shx",
  "name": "Sungrow SH Hybrid Inverter",
  "version": "1.0.0",
  "source": "bundled",
  "installed": {
    "version": "1.0.0",
    "sha256": "local-file-sha256",
    "path": "/app/drivers/sungrow.lua",
    "managed": false
  },
  "upstream": {
    "version": "1.0.1",
    "sha256": "repo-file-sha256",
    "repository_id": "hugin",
    "repository": "https://github.com/srcfl/hugin-drivers",
    "path": "drivers/sungrow.lua"
  },
  "upstreams": [
    {
      "version": "1.0.1",
      "sha256": "repo-file-sha256",
      "repository_id": "hugin",
      "repository": "https://github.com/srcfl/hugin-drivers",
      "path": "drivers/sungrow.lua"
    }
  ],
  "update_available": true
}
```

`upstream` is the best display candidate for older clients and simple
UIs. `upstreams` contains every matching repository candidate and is the
source of truth for HACS-like multi-repository selection.

Add endpoints:

| Endpoint | Purpose |
|---|---|
| `GET /api/device_repository/status` | Repository config, last refresh time, last error, cache status. |
| `POST /api/device_repository/refresh` | Force manifest refresh for all enabled repositories. No driver activation. |
| `POST /api/device_repository/repositories` | Add or update a repository entry. |
| `DELETE /api/device_repository/repositories/{id}` | Disable or remove a repository entry without touching installed drivers. |
| `POST /api/device_repository/drivers/{id}/install` | Download, hash-check, validate, activate, and optionally restart affected configured drivers. |
| `POST /api/device_repository/drivers/{id}/rollback` | Reactivate previous managed copy when available. |

Install request:

```json
{
  "repository_id": "hugin",
  "version": "1.0.1",
  "sha256": "expected-repo-file-sha256",
  "restart": true
}
```

The install endpoint must fail if the manifest hash and downloaded file
hash differ. It must also fail if the driver cannot be parsed by the
catalog loader or loaded by the Lua runtime.

## UI

Settings -> Devices should show repository status in the catalog picker
and in configured driver cards:

- source label: bundled, managed, local
- repository label for every managed/upstream candidate
- installed version and hash prefix
- upstream version and hash prefix
- update available badge
- manual Update button
- rollback button for managed drivers with a previous version
- clear error message when repository refresh fails
- repository management controls: add repository, disable repository,
  refresh repository, and show last refresh/error per repository

The UI must not imply that an update was installed just because a newer
manifest entry exists.

Update notifications should use the existing notification infrastructure
but a driver-specific event type, for example
`driver_update_available`, rather than the app-release
`update_available` event. The event payload should include driver name,
driver ID, installed version, upstream version, repository ID, repository
name, and verification status. It must not include secrets, endpoints,
serial numbers, MAC addresses, or raw config.

## hugin-drivers Requirements

The Hugin driver repository needs its own PR before forty-two-watts can
use it as a default source with confidence.

Required artifacts:

- complete `drivers/` set matching the latest bundled driver catalog
- generated `manifest.json`
- CI on PR and push
- verifier script that checks:
  - every `.lua` file has a parseable `DRIVER` block
  - every driver ID is unique
  - every version is semver
  - every manifest hash matches the file content
  - every manifest path is relative and under `drivers/`
  - every production driver has verifier metadata
  - every driver documents the site sign convention
  - every driver loads in a Lua 5.1/gopher-lua compatibility check

## Driver Fleet Statistics

Fleet statistics are useful but should be a separate opt-in PR. They are
not required for repository-based loading.

Minimum privacy contract:

- disabled by default
- preview endpoint shows the exact payload before enabling
- no serial numbers
- no MAC addresses
- no IPs, hostnames, endpoints, or URLs from local config
- no secrets or config maps
- no raw telemetry values
- include only coarse installation facts such as driver ID, driver
  version, app version, verification status, DER kinds, and anonymous
  install ID

The scrubber must have tests that fail when serial, MAC, endpoint, host,
password, token, or email-shaped values are present.

## PR Plan

### PR 1: hugin-drivers foundation

Repository: `srcfl/hugin-drivers`.

Deliverables:

- sync missing drivers from latest forty-two-watts bundled catalog
- add manifest generator
- add manifest file
- add CI verifier
- document contribution checks

Validation:

- `manifest.json` is generated from driver files
- verifier fails on duplicate IDs, bad semver, wrong hashes, unsafe paths,
  and missing production verifier metadata

### PR 2: forty-two-watts repository client

Repository: `frahlg/forty-two-watts`.

Deliverables:

- `device_repository` config with safe defaults
- multiple repository entries with the default Hugin repository
- manifest fetcher with timeout and cached last-good manifest per
  repository
- managed install directory and state table
- resolver support for activated managed copies
- additive catalog fields
- update and rollback endpoints
- Settings UI update badges and actions
- driver update-available notification event
- docs and API reference

Validation:

- existing configs parse and run without `device_repository`
- startup succeeds without network
- fake manifest server drives update-available state
- two fake manifest servers can be configured at the same time
- duplicate driver IDs from two repositories stay source-qualified in
  catalog and install requests
- hash mismatch prevents install
- path traversal in manifest is rejected
- local custom drivers are never overwritten
- installed driver is activated only after explicit POST
- failed validation keeps previous active driver
- rollback reactivates previous managed copy
- notification tests cover `driver_update_available` and do not reuse
  app-release update semantics accidentally

### PR 3: opt-in fleet statistics

Repository: `frahlg/forty-two-watts`.

Deliverables:

- disabled-by-default telemetry config
- preview endpoint
- scrubber
- anonymous install ID
- submission endpoint/client
- documentation

Validation:

- scrub tests reject serial, MAC, endpoint, host, token, password, and
  email-shaped values
- disabled config performs no outbound submission
- preview payload exactly matches submitted payload

## Completion Checklist

The device repository feature is complete when all of these are true:

- `hugin-drivers` has green CI for manifest and driver validation.
- forty-two-watts starts an existing config with no network.
- `GET /api/drivers/catalog` exposes `source`, `installed`,
  `upstream`, and `update_available`.
- The Settings UI shows update availability without installing it.
- The Update action downloads, validates, hashes, activates, and restarts
  only when requested.
- A bad manifest, bad hash, or bad Lua file cannot replace an active
  driver.
- A local custom driver path is never overwritten by repository sync.
- A custom manifest URL can be configured and tested with a fake server.
- Multiple repositories can be configured, refreshed, displayed, and
  selected from during install.
- Driver update notifications fire only when a configured driver has a
  newer compatible upstream candidate and notifications are enabled.
- The runtime container does not need `git`.
- Fleet statistics, if implemented, are opt-in and scrubbed by tests.
