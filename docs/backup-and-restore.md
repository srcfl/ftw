# Full backup and safe restore

A full backup (`.ftwbak`) recovers a site after a failed disk, a reinstall or
a release that changed the stored data, once a copy is on another disk or
computer. Local rollback does not replace it.

| Protection | Contains | Survives a failed SD card? | Used for |
|---|---|---:|---|
| Full backup (`.ftwbak`) | config, `state.db`, `history.db`, custom/managed drivers, keys and component versions | Only after copying it elsewhere | disk loss, reinstall or complete recovery |
| Previous release (native `ftw rollback`) | the previous Core release; data stays in place | No | undo an update that reads the same data |
| Older Docker installs (1.x–3.x): local rollback point | settings database and configuration; history stays in place | No | undo a Core update on that line |

A native install takes no local rollback point. `ftw rollback` returns to the
previous release with the current data, and a release that changes the state
schema is refused until its backup path exists. `ftw status` lists points an
older Core left, with their size and directory; nothing on a native install
uses them.

On older Docker installs, a Core update always creates a local rollback point
when snapshots are enabled. The point copies the settings database and
configuration only; `history.db` stays where it is. Going back across a
history-format change needs a full backup made before that update; the Update
Center refuses such a rollback and says so.

## Create and export a full backup

On a native install, run on the machine:

```bash
ftw backup --output-dir /media/usb/ftw-backups
```

Core, without stopping control:

1. makes a transactionally consistent SQLite backup;
2. exports the current config from that database snapshot and collects the
   rest of the persistent data directory;
3. records Core, Optimizer and active Driver versions;
4. hashes every file, verifies the finished archive and runs SQLite
   `quick_check` before publishing it.

`ftw backup` waits for the verified archive, copies it to the directory and
compares size and SHA-256 before the copy gets its final name. Without
`--output-dir` the archive stays only in `/var/lib/ftw/backups`, on the same
disk. Keeping a copy somewhere else is the owner's step: point `--output-dir`
at another disk, or copy the file off the machine. Keep at least one older
known-good copy.

The archive leaves out other backups, old rollback points and `cache.db`,
which holds only re-fetchable data. Managed-driver links that point inside the
persistent directory become relative links in the archive, so restore can
move the data to another directory or machine. Backup never follows these
links to copy a host file; verification rejects link chains that escape the
data directory or form a cycle.

From another computer, [`scripts/ftwctl.py`](../scripts/ftwctl.py) can do all
three steps in one command: create the archive, wait for Core's verification,
then download it and compare SHA-256 before naming the local file. Use an SSH
tunnel to the box API if it is not directly reachable:

```bash
ssh -N -L 18080:127.0.0.1:8080 box.example
python3 scripts/ftwctl.py --url http://127.0.0.1:18080 backup --output-dir ~/FTW-backups
```

The CLI prints each phase and elapsed time. It prints completed and total
bytes when Core knows both, and says `total unknown` while SQLite scans rows
without a safe total. A terminal closing before the final SHA-256 comparison
does not leave a file that looks like a finished backup. Set `FTW_API_TOKEN`
in the CLI environment if the site requires LAN auth.

Set `state.backup_dir` to a mounted external directory to keep Core's own
copies off the data disk; FTW must be able to write it.

In Docker 0.x, stop FTW and archive the whole data directory, as in
[Docker](native-beta.md#docker).

On older Docker installs (1.x–3.x), open **FTW Update Center → Full backups**,
choose **Create full backup**, then **Download** and save the `.ftwbak` file
on another computer or USB disk. **Verify** rechecks the server copy; it does
not prove that a download exists elsewhere. Archives are stored under
`data/backups/` by default, and the Update Center warns when they are still on
the live data disk.

The archive may contain credentials and household history and is created with
mode `0600`. Store it as sensitive data.

## Restore a native install

Restore is offline. Stop the service, then run `ftw-backup restore` from the
current release as the `ftw` user, as in
[When something goes wrong](native-beta.md#when-something-goes-wrong):

```bash
sudo systemctl stop ftw
sudo -u ftw /opt/ftw/releases/<current tag>/ftw-backup restore \
  -archive /var/tmp/ftw-restore.ftwbak -data /var/lib/ftw -yes
sudo systemctl start ftw
```

The archive must be readable by the `ftw` user. Restore verifies the whole
archive and extracts it before it moves anything. It keeps the replaced data,
including earlier backups on the box, in
`/var/lib/ftw/.ftw-pre-restore-<time>` and prints that path. Check the
dashboard and live device telemetry before removing it. To undo the restore,
stop the service and run `ftw-backup revert` with that path; see
[Native helper](#native-helper).

## Restore an older Docker install (1.x–3.x)

[`scripts/restore-full-backup.sh`](../scripts/restore-full-backup.sh) works
only in an install directory with the old `docker-compose.yml`; it does not
handle a native install or Docker 0.x. Copy the `.ftwbak` archive to the host
and run it from the installation directory:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/restore-full-backup.sh \
  -o /tmp/restore-ftw-backup.sh
bash /tmp/restore-ftw-backup.sh \
  --dir "$PWD" \
  --archive /path/to/ftw-full-backup-YYYYMMDDTHHMMSS.ftwbak
```

The helper verifies the entire archive before stopping anything. It stops only
Core, extracts and verifies into a staging directory, and activates the
restored contents. The previous data is retained under a safety directory.
If Core does not become healthy, the helper automatically reactivates the
previous data and checks health again. It never deletes either state while
deciding which one boots.

After a healthy restore, check the dashboard and live device telemetry before
removing the printed safety directory. A backup records component versions but
does not silently downgrade images; install a protocol-compatible Core or
Optimizer version explicitly if the diagnostics say one is required.

## Native helper

Release packages include `ftw-backup`; on a native install it is
`/opt/ftw/releases/<tag>/ftw-backup`. The offline commands are:

```text
ftw-backup create  -state /var/lib/ftw/state.db -data /var/lib/ftw -output /mnt/backup
ftw-backup verify  -archive /mnt/backup/example.ftwbak
ftw-backup inspect -archive /mnt/backup/example.ftwbak
ftw-backup restore -archive /mnt/backup/example.ftwbak -data /var/lib/ftw -yes
ftw-backup revert  -data /var/lib/ftw -safety /var/lib/ftw/.ftw-pre-restore-<time> -yes
```

Add `-progress` to `ftw-backup create` for elapsed-time JSON progress on
stderr. The final verified archive metadata remains JSON on stdout.

Stop the native FTW service before `restore` or `revert`. `create` opens the
existing database read-only and does not migrate or repair its schema.
It copies without the live 100 ms yield used while Core is collecting, and
refuses to start when the destination cannot hold the database copies, full
archive and verification extract.
Creation verifies on the output filesystem. Restore verifies on its writable
target. Standalone `verify` and `inspect` prefer the archive filesystem and
fall back to the system temporary directory when the source is read-only.
Pass `-config` to `create` when the seed has
a name other than `<data>/config.yaml`. The config seed must be inside the
data directory.

## Svenska – kortversion att skicka till en användare

1. Sätt i ett USB-minne eller anslut en annan disk till datorn där FTW körs.
2. Kör `ftw backup --output-dir /media/usb/ftw-backups` (byt till din
   sökväg).
3. Vänta tills kommandot skriver `Copied and checked:` och filens namn. Då är
   `.ftwbak`-filen kopierad och kontrollerad. Låt inte enda kopian ligga kvar
   på datorns eget SD-kort eller disk.
4. Behåll gärna två generationer. Radera först en äldre kopia när den nya är
   kopierad och kontrollerad.
5. Vid återställning: följ avsnittet om att återställa en native-installation
   ovan. `restore` sparar tidigare data i `/var/lib/ftw/.ftw-pre-restore-<tid>`
   så att den kan läggas tillbaka.

Äldre Docker-installationer (1.x–3.x) gör backup i **FTW Update Center →
Full backups** och återställer med skriptet i avsnittet för äldre
Docker-installationer.

Radera aldrig `state.db-wal`, `state.db-shm`, `history.db-wal`,
`history.db-shm` eller en safety-katalog för att försöka tvinga igång en
restore.
