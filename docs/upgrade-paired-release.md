# Upgrade a Compose install to one Core + updater tag

Use this when an existing Linux Docker Compose site already runs the
canonical `ghcr.io/srcfl/ftw` images and you want a published 3.x pair
(or any later pair). Orange **Update** recreates Core first. The first
DuckDB hop needs the new updater already running, so that button is not
the upgrade path. Choose [Svenska](#svenska) or [English](#english).

Older image names (`frahlg/forty-two-watts` and similar) still start at
[upgrade-from-legacy.md](upgrade-from-legacy.md).

This is the operator path for people who want to move. The Update Center
guard in [#1223](https://github.com/srcfl/ftw/issues/1223) does not retrofit
the 2.14 button.

---

## Svenska

### Gör inte så här

Klicka inte på orange **Update** för att gå från 2.14 till 3.x. Den
flyttar bara Core. Updatern byts först efter att nya Core är healthy, och
DuckDB-importen kan ta längre än den gamla updatern väntar.

Stanna på frisk 2.14 om du inte behöver 3.x.

### Innan du börjar

SSH till boxen. Anslut helst ett USB-minne eller en nätverkskatalog för
backupen. Den verifierade `.ftwbak`-filen måste ligga utanför `data/`.
Utan `--backup-dir` används `<installationen>/ftw-backups`, vilket inte
skyddar mot att SD-kortet går sönder.

Välj en **publicerad** tagg på
[GitHub Releases](https://github.com/srcfl/ftw/releases) som är minst
`v3.2.1-beta.1`. Använd inte `latest`, `beta` eller `v3.2.0-beta.1`.

Kör från installationskatalogen (`~/ftw` eller `~/forty-two-watts`):

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/upgrade-paired-release.sh \
  -o /tmp/ftw-upgrade.sh
bash /tmp/ftw-upgrade.sh \
  --tag v3.4.2-beta.1 \
  --dir "$PWD" \
  --backup-dir /media/$USER/FTW-BACKUP
```

Byt `v3.4.2-beta.1` mot den tagg du faktiskt vill ha. Kopiera den
utskrivna `.ftwbak`-filen till en annan dator om backupen hamnade på
samma disk.

### Tre faser

1. **Full backup.** Målbildens `ftw-backup` öppnar den nuvarande
   databasen skrivskyddat, bygger en `.ftwbak` och verifierar den innan
   någon container byts.
2. **Updater same-tag.** `FTW_UPDATER_IMAGE_TAG` pinas. Bara
   `ftw-updater` hämtas och återskapas. Skriptet kräver
   `GET /capabilities` med protocol 1 och
   `preserve_core_on_readiness_failure`. Core är kvar på 2.x tills det
   svaret finns.
3. **Core same-tag.** `FTW_IMAGE_TAG` pinas till samma tagg. Bara Core
   återskapas. DuckDB-import kan ta timmar på en Pi. Skriptet väntar upp
   till sex timmar. Misslyckad readiness rullar **inte** tillbaka bara
   imagen.

Om nya Core vägrar öppna data (`update ftw-updater first`) återställs
föregående Core. Den nya updatern lämnas kvar.

### Verifiera

```bash
docker compose images
docker compose ps
curl -fsS http://127.0.0.1:8080/api/health
curl -fsS http://127.0.0.1:8080/api/status
```

Core och updater ska visa samma tagg. Historik kan vara ofullständig
tills importen är klar; live-insamling kan redan köra. Återställning
sker med [backup-and-restore.md](backup-and-restore.md), inte genom att
byta image-tagg ensam.

---

## English

### Do not

Do not use orange **Update** to go from 2.14 to 3.x. That click moves
Core only. The sidecar replaces itself only after the new Core is
healthy, and the first DuckDB import can outlast the old updater.

Stay on healthy 2.14 if you do not need 3.x.

### Before you start

SSH to the box. Prefer a USB disk or network share for the backup. The
verified `.ftwbak` must sit outside live `data/`. Without `--backup-dir`
the script uses `<installation>/ftw-backups`, which does not survive SD
card loss.

Pick a **published** tag from
[GitHub Releases](https://github.com/srcfl/ftw/releases) that is at
least `v3.2.1-beta.1`. Do not use `latest`, `beta`, or `v3.2.0-beta.1`.

From the installation directory (`~/ftw` or `~/forty-two-watts`):

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/upgrade-paired-release.sh \
  -o /tmp/ftw-upgrade.sh
bash /tmp/ftw-upgrade.sh \
  --tag v3.4.2-beta.1 \
  --dir "$PWD" \
  --backup-dir /media/$USER/FTW-BACKUP
```

Replace `v3.4.2-beta.1` with the tag you actually want. Copy the printed
`.ftwbak` off the box if it landed on the same disk.

### Three phases

1. **Full backup.** The target image's `ftw-backup` opens the current
   database read-only, writes a `.ftwbak`, and verifies it before any
   container is replaced.
2. **Updater same-tag.** `FTW_UPDATER_IMAGE_TAG` is pinned. Only
   `ftw-updater` is pulled and recreated. The script requires
   `GET /capabilities` with protocol 1 and
   `preserve_core_on_readiness_failure`. Core stays on 2.x until that
   answer exists.
3. **Core same-tag.** `FTW_IMAGE_TAG` is pinned to the same tag. Only
   Core is recreated. DuckDB import can take hours on a Pi. The script
   waits up to six hours. A readiness failure does **not** roll back
   the image alone.

If the new Core refuses to open data (`update ftw-updater first`), the
previous Core is restored. The new updater stays.

### Verify

```bash
docker compose images
docker compose ps
curl -fsS http://127.0.0.1:8080/api/health
curl -fsS http://127.0.0.1:8080/api/status
```

Core and updater must show the same tag. History can stay incomplete
until import finishes; live collection may already run. Recover with
[backup-and-restore.md](backup-and-restore.md), not by changing only the
image tag.
