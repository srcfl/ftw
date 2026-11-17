# Move FTW with your data

Keep settings, history, identity, credentials, drivers and stored goals when
moving to the new line. Use a full backup and a separate target copy. Keep
the source install and an off-host backup until the target and the return
path have passed their checks. [Svenska](#svenska).

This draft targets `v0.142.1`, which has not shipped as stable. The published
beta is `v0.142.1-beta.1`. Data-copy tests do not establish that every old
installation can switch safely. The remaining paths below need work before
the guide can ship with stable.

## Start from the installation you have

| Source | Export and import |
|---|---|
| New 0.x native or Docker | Keep the existing data. Follow the [normal update](native-beta.md), including a verified full backup. |
| Old 0.x, 1.x, 2.x or SQLite-based 3.x | Export a full `.ftwbak`, or make a stopped copy of the entire data directory. Restore that backup on the separate target before its first configured start. |
| A DuckDB beta | Preserve the DuckDB file, hot SQLite, cold archives and settings together. Run [the exact target's offline converter](history-conversion.md) on the copied data before Core opens them. |
| Home Assistant app | Download a backup that includes FTW and keep its key. Use the [offline HA exporter](https://github.com/srcfl/ftw/releases/tag/ha-export-v1-beta.1) to check and copy its FTW component, then make a full FTW backup at the old `/data` root. The complete Supervisor route is still being qualified. |
| Only a backup remains | Identify its format and source version from its metadata. Use the checked full-backup route, or the matching old image on a separate recovery copy. |
| Custom paths, external files or another host platform | Inventory the actual mounts, config seed, databases, archives, keys, drivers and start rules first. Resolve external paths before activation. |

Record the actual source version, data path, config path and startup owner.
The old ready-made Pi image uses Docker. A source running a 32-bit OS can
export a stopped copy to a 64-bit Linux target; it cannot run the new binary.
These source/platform combinations still need their own installation tests.

Active SQLite settings take precedence over an old YAML seed. The config API
masks secrets and is not a full export. Include environment files and service
or Compose definitions needed to explain the source's paths and credentials.
Preserve shared MQTT and other services used by the equipment.

FTW Cloud needs a new owner grant after a full-backup restore. The backup
keeps pairing and WebPush keys but leaves out `cloud.key` and its temporary
file. A restored Cloud grant reads as off until the owner turns Cloud on
again under a new home ID. Local operation and stored goals remain.

## Export a complete source copy

Use the source version's full-backup UI or helper when it has one. Verify the
finished archive and copy it off the source host. A verified server copy does
not establish that the downloaded file has the same bytes.

For a source without a helper, stop Core and preserve its whole data tree,
including SQLite WAL files, keys, drivers and required archives. On a
separate 64-bit Linux copy, the target's `ftw-backup create` can open legacy
SQLite read-only without upgrading it. Pass `-core-version` with the verified
**source** version and `-config` if the config seed has another name. Keep
external files and their path mapping explicit; the helper requires its
config seed inside the data tree.

For an external layout, keep the stopped source and its config seed intact.
Use a second writable copy to collect the database, required archives, keys
and driver files inside one data tree. Map each known file path in that
copy's active SQLite config and YAML seed; keep all other settings and
credentials. If the source has no stored config, keep YAML as its source
until Core imports it. Do not add a `config_database` reference to a database
that has no stored config. Verify the resulting full backup and restore it
on a separate target. An unresolved external path must stop the move.

Do not start an old Core merely to inspect the migration copy. Preserve the
original bytes and use the full-backup inspector or converter. When a format
or source is unknown, keep the copy and resolve that case before starting it.

For a Home Assistant backup, follow the exporter's included README. It needs
Linux and Python 3.11 or later, checks plain and protected v2/v3 components,
and keeps checked internal driver links. It refuses outside links and unknown
layouts. The extracted copy must be writable when the FTW helper opens old
SQLite; WAL/SHM sidecars may be needed. Keep the original backup unchanged.
Use the printed source version in `ftw-backup create -core-version`, and mount
the extracted data at `/data` when making the full backup. That preserves the
source root for verified relocation; a host path is not a replacement for it.

## Restore a full backup on a new native target

Prepare a new Linux host or Pi card with [the exact release package](native-beta.md#install).
The installer starts an empty setup service. Stop and disable it before
import, so it cannot start configured Core during an unfinished move.

Copy the verified full backup to `/var/tmp/ftw-move.ftwbak`. Use the exact tag
installed on this target in place of `v0.X.Y`. The following block verifies,
restores and fixes ownership. It leaves Core stopped:

```bash
(
  set -eu
  tag=v0.X.Y
  archive=/var/tmp/ftw-move.ftwbak
  helper=/opt/ftw/releases/$tag/ftw-backup
  sudo test -x "$helper"
  sudo systemctl disable --now ftw
  sudo "$helper" verify -archive "$archive"
  sudo "$helper" restore -archive "$archive" -data /var/lib/ftw -yes
  sudo chown -R ftw:ftw /var/lib/ftw
)
```

Restore checks the archive before replacing data and prints the retained
previous-data directory. Keep that path. It maps archive-internal config
paths to `/var/lib/ftw`, while keeping other stored settings. A directory move
with unresolved external paths stops before activation. Do not remove the
path check to make a restore pass.

For DuckDB, complete the offline conversion now, using the matching converter
package and all copied sources. Keep Core stopped until it succeeds. A file
that changed during conversion stops resume; keep the original and follow
the converter's diagnosis.

For a new Docker target, use its separate Compose project and the
[verified restore recipe](native-beta.md#update-new-docker). Keep Core
stopped through any conversion. Do not attach new Core to the source's live
data volume.

## If import or return stops

Keep both Core processes and their autostart off. Retain the full backup and
all active, staging and safety directories. Do not delete a hidden directory
or bypass a check to make the command succeed.

The new helper checks a restore point's complete file list before reverting.
It refuses a partial point, an older point without that record, or a retry
that would treat a partly moved active directory as complete. Recover the
verified full backup into a separate directory instead; retain the current
files too, including readings written after that backup.

If recovery returns `safety_complete: false`, the previous complete data have
returned, but the retained rejected target is partial. Keep it for inspection;
do not start Core from it or try to switch back to it. Check the recovered
installation and its start rules before enabling control.

## Switch control and verify

1. Stop old Core and its updater. Disable the confirmed old systemd, Compose,
   timer or custom startup path. In Home Assistant, also disable **Start on
   boot** and **Watchdog**. Keep needed MQTT running.
2. Confirm that the old process has stopped. A port error or one visible
   dashboard does not establish that only one Core runs.
3. Start the prepared target. On native, run
   `sudo systemctl enable --now ftw`. Use the saved API port; migration keeps
   a custom port too.
4. Check the actual running version and binary, saved settings and credentials,
   key identity, goals, representative historical periods, archive exports,
   fresh device readings and new history writes. Check the plan and actual
   equipment feedback before accepting control.
5. At a safe time, reboot the target and check again that old Core stays off,
   exactly one Core runs, the imported data remain and new writes continue.

If a check fails, stop new Core, preserve its data and diagnose. Return with
the matching old version and its saved data, with new Core stopped. Restore
only the source start rules recorded before the move. Keep data collected on
the target; returning does not merge those readings into old Core.

## What the copy tests cover

The published `v0.142.1-beta.1` Core passed healthy, offline and invalid
balance cases over two starts each on separate synthetic Linux copies.
Known meter values remain in history while missing flows and totals stay
unknown. Energy today shows measured subtotals with a gap mark; the full
range remains unknown when measurements are incomplete. It also records the time spent in forecast connection, query, scan
and decode phases when a read fails. The timeout cause remains unproven.
These checks alone do not qualify the older migration routes against this beta.

The published `v0.142.1-beta.1` helper also passed full backup, verification,
portable restore and revert on separate copies of published HA 3.8 split
SQLite and current partial/quality history. Historical rows, settings and
identity remained after explicit data-root and restore-generation mappings.
Revert kept new target writes in its safety directory. These are data-copy
checks; the HAOS, MQTT and installation tests below retain their own versions.

The same published Core and helper passed a separate Cloud restore check
without network access. Core created a local grant on the source copy. A
verified full backup omitted the Cloud key, and two restored starts kept
Cloud off. A new explicit grant used a different home ID. Source keys and
modes, pairing identity and WebPush identity remained. This tests local
restore behavior, not a connection to the Cloud service.

The published `v0.142.1-beta.1` Core, helper and converter also passed 30
checks from published `v3.2.0-beta.1` with retained Parquet and 500,000 extra
synthetic DuckDB rows. A process kill interrupted the copy before publication;
resume checked the source, and a changed source stopped it. Two target
starts and two portable-restore starts kept history, goals, settings and
identity. Return and revert retained both copies. This does not qualify
an installation change, Pi/SD performance or physical power loss.

A separate protected full backup and full return passed on a separate HAOS 18.3
ARM64 VM, with Supervisor 2026.09.3 reporting supported and healthy. Its
published HA 3.8 FTW source used synthetic data. Export and full FTW backup
with the exact published `v0.141.4-beta.1` Core/helper passed two target
starts, new writes and revert on a separate Linux ARM64 data copy. Old
chart rows, two archive hours, settings, credentials, goals and identity
remained. The checks verify root relocation and the older planner and SoC
field mappings rather than requiring those fields to keep their old form.
Native installation and physical cutover need separate proof.

The protected HAOS return restored a synthetic template sensor's entity ID,
unique ID and state, and both HA and FTW test files. Old FTW still needed
the owner recovery command below before it could start. That command kept
all 61 checked files' bytes and modes and both internal links. The matching
old Core then returned with its chart rows and two archive hours. The VM
and FTW test targets are stopped; their backups and data remain.
This template check does not test FTW MQTT entities, every community app,
real equipment or power-loss recovery.

A separate MQTT move on that own HAOS VM kept the same broker running while
only the selected FTW Core ran. The published HA 3.8 source and exact
`v0.141.4-beta.1` target kept all 44 recorded MQTT entity IDs, unique IDs
and device links through two target starts and matching old return. The same
grid sensor reported 700 W, then the target's new synthetic 800 W, then the
old source's 700 W. Old chart rows and both stored hours remained; new ticks
had no failed or rejected writes. Both full backups remain off the VM.

The checks compare credentials and identity without exporting their values.
They verify the old Balanced forecast margin's move to the current Balanced
style, alongside the root, planner and SoC mappings. A guest OS reboot with
the old app's Start on boot, Watchdog and auto-update off started neither
old Core nor the removed target. This covers one synthetic MQTT setup and a
Docker data target on HAOS. It does not qualify every entity or command,
community layout, native installer, real device or power-loss case.

The published `v0.141.2-beta.1` Core and helper preserve partial history in
a full backup. Small synthetic meter/battery cases passed two starts,
portable restore and return: a working meter keeps recording while the
battery is offline, and missing flows and totals stay unknown. A balance
warning also leaves measured flows in the record. The browser shows the
gap and warning, then new values after recovery. These checks do not qualify
the older installation routes below against this newer beta; their actual
tested target remains `v0.141.0-beta.1`.

The published `v0.141.3-beta.1` helper also exports the older split SQLite
history, whose schema has none of the six newer partial/quality tables.
It refuses an incomplete set. Actual plain and protected Supervisor full
backups from a synthetic published HA 3.8 source passed export, portable
restore, two starts per route and revert with the published 0.141.3 packages.
Old charts, stored hours, config, credentials, goals, identity and managed
driver choices remained. The trial used the official unsupported Supervisor
devcontainer, not a supported HAOS host or real equipment.

The full Supervisor return job restored files, but the old app failed to
start because its private files had the wrong owner. The separate
[owner recovery command](https://github.com/srcfl/home-assistant-addons/blob/main/docs/restore-data-owner.md)
now provides a checked route for that failure. Run it in the stopped app's
matching image with only its verified data mounted. It preserves bytes,
private file modes and internal links, and stops on external or broken links,
hard links or extra mounts. Keep Start on boot and Watchdog off; record the
image and data path before Supervisor removes the stopped container.

A complete protected return job on that separate test instance passed the
command, old Core start, retained chart rows and two archive hours, then a
final stop. Its source was the same published HA 3.8 image. This command
does not publish or update the retired app line.
Automatic Supervisor return remains unqualified. This older devcontainer
check did not test FTW MQTT entities.
Keep the original app and its data; do not treat a completed backup job as
proof that the app can return without help.

The published `v0.141.0-beta.1` packages have passed representative
SQLite/settings copies from 0.130.4, 1.15.0, 2.3.2 and 3.8.0-beta.1.
The matching Core and converter pair also passed DuckDB copies from
3.2.0-beta.1 and 3.4.2-beta.4. Each test uses the published old binary or
image and synthetic data. The checks retain exact old chart rows, settings,
credentials, goals and identity over two target starts, new writes and
return to the matching old Core.

Those four SQLite sources also passed full-backup restore to both the same
data path and a new path with the published target helper. Two synthetic
Parquet rows in the old 2.x long format retain their bytes and JSON/CSV
exports over two target starts. Stored settings, credentials, goals,
identity and new writes remain, and revert returns the previous target.
These small cases do not cover every older cold format or external file.

A separate 2.3.2 archive test passed 37 checks with 1,036,800 synthetic
Parquet values in ten daily files and twelve series. Restore to the same
and a new root retained exact hourly counts, sums, minimums, maximums and last timestamps
over two starts. JSON/CSV exports, settings, goals, identity, new writes,
revert and raw exports from the matching old Core passed. The compressed
files total about 9 MiB; this tests row count on Linux ARM64 in Docker,
not a large disk footprint, Pi/SD performance or physical power loss.

DuckDB tests include hot SQLite, imported or retained long-format Parquet,
500,000 extra raw values, interrupted conversion and resume, generation
checks and portable backup/restore/revert. These copies do not qualify every
old schema, archive format, platform or complete installation change.

Supervisor-format tests cover plain, compressed and encrypted version 2/3
components using the published 3.8 HA image. The offline exporter and `v0.141.0-beta.1`
copy tests also cover synthetic managed-driver choices and internal links.
They do not cover a complete Supervisor job, every community app, a signed
driver-channel move or HA entity continuity. An isolated Linux systemd test
does not establish a physical Pi cutover or a power-loss recovery.

Four separate managed-driver copy tests also passed with the published
`v0.141.0-beta.1` helper and Core. Current and older default manifests,
same and new data roots, two starts and revert keep the selected driver
active and preserve its previous-version path. These checks use synthetic
records and valid file hashes; they do not prove a signed-channel install.

A later two-host test passed 39 checks from the published 3.8 Core and
helper to the published `v0.142.1-beta.1` native package. The unchanged
installer refused the source's existing `ftw` account and installed on a
fresh Debian 13 ARM64 target. Full backup transfer, two target starts and
matching old return retained 24 exact chart rows, two stored hours, settings,
credentials, goals and keys. The check compared every selected old config
field after explicit path, planner, SoC and forecast-margin mappings.
The source data stayed intact and new target writes had no errors.

Real systemd rules kept only the selected Core running after both test
machines restarted, including after return. Both stopped full backups were
verified off the guest, and both machines ended stopped with data retained.
These OrbStack machine restarts test Linux service startup, not a physical
Pi power cycle. The source has no Parquet files; this test does not add
Parquet coverage, every custom layout, in-place installation or equipment
control.

A separate two-host Linux ARM64 test moved a synthetic 3.8 custom systemd
source to a fresh `v0.141.0-beta.1` native target. The exact published old
helper created the full backup. Both hosts restarted with only the selected
Core enabled. The checks retained the source's exact chart rows and stored
hours, settings, credentials, goals and keys, verified new writes, and
returned to the matching old binary while keeping the target data. The
installer ran on the fresh target; this does not qualify an in-place
install, every custom service, physical equipment or a Pi power cycle.

A separate same-host test used the [native pilot](self-update.md#pilot-an-older-native-systemd-site)
with a synthetic 3.8 systemd source: `/opt/ftw/ftw`, the `ftw` user and a
data bind. The corrected operator CLI and published `v0.141.0-beta.1`
package passed 24 checks. Charts, stored hours, settings, credentials,
goals, identity and new writes survived the switch and a machine restart
with one Core. Return to the matching old Core passed another restart,
and both off-host backups remain. This covers that Linux pilot layout;
it does not qualify every native install or physical power loss. The
normal fresh-host installer still refuses an existing installation.

The published 2.3.2 Docker image also passed a move to `v0.141.0-beta.1`
using two Compose projects and separate named data volumes. A synthetic
MQTT meter and an independent subscriber used the same broker throughout
backup, two target starts and return to old Core. The checks retained exact
old chart rows, settings, credentials, goals, identity and Lua files, and
verified new writes. The broker and subscriber kept their process start
times. The test checked saved Compose and container restart policies; it
did not reboot Docker or the host, run the old updater, or control equipment.

A later ARM64 Docker test passed 62 checks from the published 2.3.2 Core
and updater to the exact published `v0.141.4-beta.1` Core/helper. The old
updater answered on its Unix socket. Separate data trees and saved Compose
and container policies kept old Core and updater off while the beta target
continued writing after a verified Docker daemon restart and guest OS
reboot. Shared MQTT recovered with fresh synthetic readings. Both target
starts and old return retained the exact 25 source chart rows, two Parquet
values, settings, credentials, goals and identity. Full backups are verified
off the VM, including the final stopped target data; revert keeps new data.

The test used an own QEMU/HVF HAOS guest as a Docker host. Supervisor marked
standalone images as `unsupported/software`; its normal check returned to
supported and healthy after cleanup. This does not qualify a standalone FTW
Docker installation as a supported HAOS app, every host startup unit,
old updater jobs, the optional old optimizer, physical equipment or power
loss. The test VM is shut down and all proof containers are removed.

A separate test moved the published 2.3.2 AMD64 Docker image to the published
`v0.141.0-beta.1` ARM64 Core and helper. All 29 checks passed, including two
target starts, new writes and return to old Core with data, settings,
credentials, goals, identity and shared MQTT retained. Both binary architectures
were checked. The AMD64 source ran through emulation on Apple Silicon.
This covers the tested Linux Docker copy across CPU architectures; it does
not qualify 32-bit sources or native macOS/Windows paths.

A separate test passed 42 checks from one custom Linux 386 build of the
unchanged 2.3.2 source to the published `v0.142.1-beta.1` ARM64 Core and
helper. The ELF32 source ran under QEMU user emulation and wrote current
history with millisecond timestamps. A verified full backup from a stopped,
writable export copy kept the source unchanged. Two ARM64 starts retained
25 exact old chart rows, two Parquet values, stored goals and credentials,
identity and all old YAML config fields after checked path, planner, SoC
and PV-unit mappings. New writes had no errors or rejected ticks. Return
to the matching 32-bit build and revert passed; the old source, backups
and new target database remain. All test containers are stopped.
This is a data-copy check for that custom build. It is not a published
32-bit package, an ARMv7/Pi test, a 32-bit OS, an installer change, reboot
or physical power-loss proof.

A native macOS ARM64 copy test passed 45 checks from an own Mach-O build
of the unchanged 2.3.2 source to the published `v0.142.1-beta.1` Linux
ARM64 Core and helper. The macOS process wrote new SQLite rows with all
network access denied; its source HTTP API was blocked too. A separate,
stopped Linux export copy kept the original macOS data unchanged. Two Linux
starts retained 25 exact old chart rows, two Parquet values, stored goals
and credentials, identity and all old YAML config fields after checked
path, planner, SoC and PV-unit mappings. New writes, native macOS return
and revert passed. Backup bytes were checked outside the export container,
on the same physical Mac. All source and target processes are stopped.
This covers that custom data-copy layout. It is not a published macOS
package, source API or installer test, Windows migration, a backup on a
separate physical host, equipment or power-loss proof.

One external Linux layout from published 2.3.2 also passed 36 checks with
the published `v0.141.4-beta.1` ARM64 Core and helper. Its database used a
subdirectory, while the config seed, driver files and Parquet archive used
other paths. The helper refused an external config seed and stopped a move
with unresolved paths before replacing target data. A separate copy with
explicit path mappings retained 25 old chart rows, two Parquet values,
settings, credentials, goals, keys and driver files over two target starts
and return to old Core. The check compared every old config field, with
explicit mappings for paths, old planner choice and units. Revert kept new
target data. This covers that stopped-copy layout, not every external mount,
an installer change, physical equipment or power loss.

The published `v0.141.0-beta.1` Linux ARM64 backup helper also passed process
crash tests during preservation, activation, return and a later extraction.
The tests check that incomplete copies cannot replace active data, all old
and new files remain, and a complete backup can restore on a separate target.
These tests use synthetic files and do not prove recovery from physical
power loss or an interrupted installation change.

Large archives on Pi/SD, other external layouts, cross-platform and other 32-bit sources,
complete Supervisor control, interrupted installation changes and physical cutover
remain release requirements. Unknown formats keep their source data and
require a checked import path.

## Svenska

Ta med dina inställningar och data. Börja med en full backup av gamla FTW
och behåll den gamla installationen. Kontrollera filen efter kopiering till
en annan disk eller dator. Använd en separat värd eller ett nytt Pi-kort som mål.

1. Identifiera gamla versionen, dataformatet och de faktiska filvägarna.
   Aktiva SQLite-inställningar gäller före gammal YAML. Maskerad API-config
   saknar hemligheter och räcker inte som backup.
2. Exportera allt som behövs: data, config, lösenord, nycklar, mål,
   drivrutiner och arkiv. Home Assistant-backup behöver sin nyckel. Använd
   den länkade exportören för FTW-komponenten och följ dess README för full
   backup med den gamla roten `/data`.
   FTW Cloud kräver ett nytt godkännande efter full återställning. Backupen
   utelämnar Cloud-nyckeln; återställd Cloud är avstängd tills ägaren slår på
   den igen med ett nytt home-ID. Pairing, WebPush och sparade mål består.
   Ligger filer på externa sökvägar: behåll den stoppade källan och samla
   filerna i en extra skrivbar kopia. Mappa bara kända sökvägar i kopians
   aktiva SQLite-config och YAML-seed. Behåll övriga inställningar. Lägg
   inte till `config_database` om databasen saknar lagrad config. Olösta
   externa sökvägar ska stoppa flytten före aktivering.
3. Installera exakt målversion på den separata Linuxmaskinen. Stoppa och
   stäng av nya tjänstens autostart före import. Använd blocket ovan för en
   verifierad `.ftwbak`; det lämnar Core stoppad.
4. Har källan DuckDB, kör målversionens konverterare på kopian innan Core
   startar. Vid ett fel: behåll båda kopiorna och lös felet.
5. Stoppa gamla Core och updater samt deras autostart. Stäng också av Home
   Assistants **Start on boot** och **Watchdog**. Behåll nödvändig MQTT.
6. Starta nya Core. Kontrollera config, mål, identitet, gamla tidsperioder,
   arkiv, färska enhetsvärden, plan och nya skrivningar. Upprepa efter en
   planerad omstart och kontrollera att gamla Core fortfarande är avstängd.

Vid ett avbrott eller fel: håll båda Core och deras autostart avstängda.
Behåll backupen och alla data-, staging- och återställningskataloger. En
ofullständig eller äldre återställningspunkt utan kvitto ska inte tvingas
igång. Återställ fullbackupen till en separat katalog och behåll också data
som skrevs efter backupen. `safety_complete: false` betyder att den sparade
avvisade målkopian är ofullständig; använd den inte för en ny start.

Vid återgång: stoppa nya Core först och använd rätt gammal version med dess
sparade data. Behåll också nya data. Guidens provade datafall är avgränsade;
hela flytten från Supervisor, egna layoutar och olika värdplattformar behöver
fortfarande egna prov före stable.
