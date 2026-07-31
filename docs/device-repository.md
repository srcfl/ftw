# Driver source and signed releases

[`srcfl/device-drivers`](https://github.com/srcfl/device-drivers) is FTW's main
driver source and default signed channel. FTW does not run raw code from the
repository branch. The release workflow builds a read-only FTW artifact for
each catalog driver from a reviewed `main` commit, signs one manifest and
publishes the files through GitHub Releases.

Device Support may later consume an exact public commit for another product or
a higher support level. That path does not own a second editable driver copy
and does not replace FTW's default channel.

## Resolution and recovery

A configured driver resolves in this order:

1. operator-owned local override;
2. explicitly activated managed artifact;
3. bundled recovery driver.

Refreshing the signed manifest only updates discovery data. It never installs,
activates or restarts a driver. FTW verifies the Ed25519 signature, driver ID,
SemVer, host API range, URL, file size and SHA-256. Installation then compiles
the Lua file and checks its metadata before it switches only that driver's
active symlink.

SQLite records the active and previous content-addressed files. During an
update, Core sends the safe default mode, restarts the driver and waits for
fresh telemetry and the same hardware identity. A failed check restores the
last verified version. Repository or network failure cannot block Core from
starting with bundled drivers.

## Independent driver versions

Each release asset has this shape:

```text
driver-<id>-v<major.minor.patch>-<sha256-prefix>.lua
```

FTW first downloads the small `manifest.json`. When the operator installs or
updates one driver, FTW downloads only that asset. Other driver files do not
change. The manifest retains older signed entries in `history`, so the Update
Center can select or restore an exact version without a Core release.

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

Set `device_repository: { enabled: false }` to opt out. Test beta on one chosen
site by changing the ID, name and URL to:

```yaml
    - id: ftw-device-drivers-beta
      name: FTW device drivers beta
      format: ftw.manifest/v1
      manifest_url: https://github.com/srcfl/device-drivers/releases/download/drivers-beta/manifest.json
      enabled: true
      trusted_keys:
        ftw-drivers-2026-01: MX+j27UBkyM099hTyJlmMLK9qlTTDUJsaK/vH12fFKc=
```

Beta receives reviewed `main` commits. Stable promotion accepts only the exact
commit named by the signed beta manifest. It rebuilds the signed channel for
stable but requires unchanged driver bytes unless that driver's SemVer rises.
There is no edge channel.

## Runtime trust

The signed public channel is read-only. Each manifest entry binds the driver
artifact, source commit and only the read permissions it needs:

- `http.get`;
- `modbus.read`;
- `mqtt.subscribe`;
- `serial.read`.

FTW binds those permissions to the active managed file. It denies write calls
during init, poll, command, default mode and cleanup. The release build also
makes the Lua artifact write-inert. These checks do not claim hardware test
coverage; the public catalog and support status hold that evidence.

Remote Lua never runs from a URL. Local unsigned drivers need an explicit
operator file and never claim signed or managed status. Bundled drivers remain
the offline recovery set.

FTW still understands `sourceful.driver-index/v1` for later signed Device
Support packages. That format is optional and is not the default source.
