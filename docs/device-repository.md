# Driver source and signed releases

[`srcfl/device-drivers`](https://github.com/srcfl/device-drivers) is FTW's main
driver source and default signed channel. FTW does not run raw code from the
repository branch.

Drivers reach a site with the Core release. Each release bundles the commit
pinned in [`drivers/BUNDLED_SOURCE.json`](../drivers/BUNDLED_SOURCE.json), and
`ftw update` and `ftw rollback` move those drivers with Core. The signed
channel serves installs that cannot take a new Core, such as 1.x–3.x, and lets
an expert try one driver ahead of a release. Its release workflow builds an FTW
artifact for each catalog driver from a reviewed `main` commit, signs one
manifest and publishes the files through GitHub Releases.

Device Support may later consume an exact public commit for another product or
a higher support level. That path does not own a second editable driver copy
and does not replace FTW's default channel.

## Resolution and recovery

A configured driver resolves in this order:

1. operator-owned local override;
2. explicitly activated managed artifact;
3. bundled recovery driver.

Drivers ship with the release. A managed artifact the owner selected runs
while it is at least as new as the release's own copy at the same path, or
when the owner went back to it from a newer version that was running. When a
release brings a newer copy, that copy runs instead, but the selection is
kept: `driver-repository/active` records what the owner selected, and
`driver-repository/effective`, which paths resolve through, is derived from it
and the release's drivers at every start and after every change. A rollback,
or an update trial that falls back, therefore runs the selection again.
`ftw status` lists the version each configured driver runs and, for an
override, the release's own version.

Settings → Devices is the one place a driver version is seen and changed.
Each device's Versions list shows the release's copy, the signed stable
versions and any beta newer than stable, with a link to what changed. Nothing
there announces a new version; the owner checks and picks.

Refreshing the signed manifest only updates discovery data. It never installs,
activates or restarts a driver. FTW verifies the Ed25519 signature, driver ID,
SemVer, host API range, URL, file size and SHA-256. Installation then compiles
the Lua file and checks its metadata before it switches only that driver's
active symlink.

SQLite records the active and previous content-addressed files. During an
update, Core sends the safe default mode, restarts the driver and waits for
fresh telemetry and the same hardware. A newly reported serial or MAC must
retain the previous MAC or endpoint as matching evidence. Missing evidence
keeps verification pending; conflicting evidence fails the update.
A failed check restores the
last verified version. Core matches the signed driver ID to bundled metadata even when filenames
differ, and saves the selected path after the runtime checks pass. Disabled
or stopped instances do not count as verified updates. The API returns
`runtime_verified` and `restarted_drivers` so clients can distinguish an
installed artifact from a running update. When `config_changed` is true,
reopen Settings before saving other edits; the old config revision cannot
overwrite the saved driver selection. Repository or network failure cannot
block Core from starting with bundled drivers.

## Independent driver versions

Each release asset has this shape:

```text
driver-<id>-v<major.minor.patch>-<sha256-prefix>.lua
```

FTW first downloads the small `manifest.json`. When the operator installs or
updates one driver, FTW downloads only that asset. Other driver files do not
change. The manifest retains older signed entries in `history`, so Settings →
Devices can select or restore an exact version without a Core release.

The publisher does not replace content-addressed driver assets. GitHub can
therefore retain each asset's `download_count`. The public repository includes
`tools/ftw_download_stats.py` to report counts by driver, version and channel.
GitHub counts downloads, not unique users or active installs.

## Default and beta channels

FTW enables signed stable discovery by default with this pinned source:

```yaml
device_repository:
  enabled: true
  refresh_interval_h: 24
  repositories:
    - id: ftw-official
      name: FTW device drivers
      format: ftw.manifest/v1
      manifest_url: https://github.com/srcfl/device-drivers/releases/download/drivers-stable/manifest.json
      enabled: true
      trusted_keys:
        ftw-drivers-2026-01: MX+j27UBkyM099hTyJlmMLK9qlTTDUJsaK/vH12fFKc=
```

Set `device_repository: { enabled: false }` to opt out. The beta channel is
built in and never replaces the stable source. Install one driver from it on
one chosen site:

```bash
curl -X POST http://127.0.0.1:8080/api/device_repository/drivers/easee_cloud/install \
  -H 'Content-Type: application/json' -d '{"channel":"beta","version":"1.3.3"}'
```

Beta receives reviewed `main` commits. Stable promotion accepts only the exact
commit named by the signed beta manifest. It rebuilds the signed channel for
stable but requires unchanged driver bytes unless that driver's SemVer rises.
There is no edge channel.

## Runtime trust

Each manifest entry binds the driver artifact, its source commit and its
permissions. A driver the catalog marks `control: true` keeps its control path
and runs under the same terms as the copy bundled with Core, which is the same
source. Every other entry gets only the read permissions it needs:

- `http.get`;
- `modbus.read`;
- `mqtt.subscribe`;
- `serial.read`.

FTW binds those permissions to the active managed file. It denies write calls
during init, poll, command, default mode and cleanup, and the release build
makes that Lua artifact write-inert. These checks do not claim hardware test
coverage; the public catalog and support status hold that evidence.

Remote Lua never runs from a URL. Local unsigned drivers need an explicit
operator file and never claim signed or managed status. Bundled drivers remain
the offline recovery set.

FTW still understands `sourceful.driver-index/v1` for later signed Device
Support packages. That format is optional and is not the default source.
