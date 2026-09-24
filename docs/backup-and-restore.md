# Full backup and safe restore

FTW has two different safety nets. They solve different failures and the UI
names them separately:

| Protection | Contains | Survives a failed SD card? | Used for |
|---|---|---:|---|
| Local rollback point | settings database and configuration; history stays in place | No | quickly undo a Core update or state rollback |
| Full backup (`.ftwbak`) | all persistent data, cold history, custom/managed drivers and component versions | Only after downloading/copying it elsewhere | disk loss, reinstall or complete recovery |

A Core update always creates a local rollback point when snapshots are enabled.
There is no skip control in the UI, and old clients cannot disable the server's
rollback point with `skip_snapshot`. Local points remain on the same disk, so
they are not a substitute for an exported full backup.

A native install takes no local rollback point: `ftw rollback` returns to the
previous release with the current data, and a release that changes the state
schema is refused until its backup path exists. `ftw status` lists points an
older Core left, with their size and directory; nothing on a native install
uses them.

The point copies the settings database and configuration only. `history.db`
stays where it is: the update does not replace it and a rollback leaves it in
place. The step is therefore bounded by the size of the settings, not by years
of telemetry, and takes minutes on a Raspberry Pi. Going back across a
history-format change needs a full backup made before that update; the Update
Center refuses such a rollback and says so.

## Create and export a full backup

Open **FTW Update Center → Full backups** and choose **Create full backup**.
FTW:

1. makes a transactionally consistent SQLite backup without stopping control;
2. exports the current config from that database snapshot and collects the rest of the persistent data directory;
3. records Core, Optimizer and active Driver versions;
4. hashes every file, verifies the finished archive and runs SQLite
   `quick_check` before publishing it.

Managed-driver links that point inside the persistent directory become relative
links in the archive. Restore can therefore move the data to another directory
or machine. Backup never follows these links to copy a host file; verification
rejects link chains that escape the data directory or form a cycle.

Choose **Download**, save the `.ftwbak` file on another computer or USB disk,
and keep at least one older known-good copy. **Verify** rechecks the server copy;
it does not prove that a download exists elsewhere.

A native install has no backup controls in the web UI. On the machine, run:

```bash
ftw backup --output-dir /media/usb/ftw-backups
```

It waits for Core's verified archive, copies it to the directory and compares
size and SHA-256 before the copy gets its final name. Without `--output-dir`
the archive stays only in Core's backup directory on the same disk. Keeping a
copy somewhere else is the owner's step: point `--output-dir` at another
disk, or copy the file off the machine.

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

The default Compose installation stores on-device archives under
`data/backups/`. Set `state.backup_dir` to a mounted external directory if that
directory is available to the FTW container. The Update Center warns when the
archive is still on the live data disk.

The archive may contain credentials and household history and is created with
mode `0600`. Store it as sensitive data.

## Restore a Compose installation

Copy the wanted `.ftwbak` archive to the FTW host. From the installation
directory, download the reviewed restore helper and run it:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/restore-full-backup.sh \
  -o /tmp/restore-ftw-backup.sh
bash /tmp/restore-ftw-backup.sh \
  --dir "$PWD" \
  --archive /path/to/ftw-full-backup-YYYYMMDDTHHMMSS.ftwbak
```

The helper verifies the entire archive before stopping anything. It stops only
Core, extracts and verifies into a staging directory, and atomically activates
the restored contents. The previous data is retained under a safety directory.
If Core does not become healthy, the helper automatically reactivates the
previous data and checks health again. It never deletes either state while
deciding which one boots.

After a healthy restore, check the dashboard and live device telemetry before
removing the printed safety directory. A backup records component versions but
does not silently downgrade images; install a protocol-compatible Core or
Optimizer version explicitly if the diagnostics say one is required.

## Native helper

Release archives include `ftw-backup`. The offline commands are:

```text
ftw-backup create  -state /var/lib/ftw/state.db -data /var/lib/ftw -output /mnt/backup
ftw-backup verify  -archive /mnt/backup/example.ftwbak
ftw-backup inspect -archive /mnt/backup/example.ftwbak
ftw-backup restore -archive /mnt/backup/example.ftwbak -data /var/lib/ftw -yes
ftw-backup revert  -data /var/lib/ftw -safety /var/lib/.ftw-pre-restore-... -yes
```

Add `-progress` to `ftw-backup create` for elapsed-time JSON progress on
stderr. The final verified archive metadata remains JSON on stdout.

Stop the native FTW service before `restore` or `revert`. `create` opens the
existing database read-only and does not migrate or repair its schema.
It copies without the live 100 ms yield used while Core is collecting, and
refuses to start when the destination cannot hold the database copies, full
archive and verification extract, including cold Parquet and other saved files.
Creation verifies on the output filesystem. Restore verifies on its writable
target. Standalone `verify` and `inspect` prefer the archive filesystem and
fall back to the system temporary directory when the source is read-only.
Pass `-config` to `create` when the seed has
a name other than `<data>/config.yaml`. The config seed must be inside the
data directory.

## Svenska – kortversion att skicka till en användare

1. Öppna **FTW Update Center → Full backups**.
2. Tryck **Create full backup** och vänta tills den står som verifierad.
3. Tryck **Download** och spara `.ftwbak`-filen på en annan dator eller ett
   USB-minne. Låt inte enda kopian ligga kvar på Raspberry Pi:ns SD-kort.
4. Behåll gärna två generationer. Radera först en äldre kopia när den nya är
   nedladdad och verifierad.
5. Vid återställning: kopiera filen till Pi:n och använd kommandot i avsnittet
   ovan. Skriptet provar den återställda installationen och lägger automatiskt
   tillbaka tidigare data om hälsokontrollen misslyckas.

Radera aldrig `state.db-wal`, `state.db-shm`, `cold/` eller en safety-katalog
för att försöka tvinga igång en restore.
