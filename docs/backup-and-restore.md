# Full backup and safe restore

FTW has two different safety nets. They solve different failures and the UI
names them separately:

| Protection | Contains | Survives a failed SD card? | Used for |
|---|---|---:|---|
| Local rollback point | consistent SQLite database and configuration | No | quickly undo a Core update or state rollback |
| Full backup (`.ftwbak`) | all persistent data, cold history, custom/managed drivers and component versions | Only after downloading/copying it elsewhere | disk loss, reinstall or complete recovery |

A Core update always creates a local rollback point when snapshots are enabled.
There is no skip control in the UI, and old clients cannot disable the server's
rollback point with `skip_snapshot`. Local points remain on the same disk, so
they are not a substitute for an exported full backup.

## Create and export a full backup

Open **FTW Update Center → Full backups** and choose **Create full backup**.
FTW:

1. makes a transactionally consistent SQLite backup without stopping control;
2. collects the rest of the persistent data directory;
3. records Core, Optimizer and active Driver versions;
4. hashes every file, verifies the finished archive and runs SQLite
   `quick_check` before publishing it.

Choose **Download**, save the `.ftwbak` file on another computer or USB disk,
and keep at least one older known-good copy. **Verify** rechecks the server copy;
it does not prove that a download exists elsewhere.

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

Stop the native FTW service before `restore` or `revert`. `create` opens the
existing database read-only and does not migrate or repair its schema.

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
