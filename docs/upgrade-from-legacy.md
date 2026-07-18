# Upgrade an older installation to Sourceful FTW

Use this guide for an existing Linux Docker Compose installation with a
`docker-compose.yml`. The migration preserves its directory, Compose project,
service name, configuration, database, history, hardware identities and
persistent `data/` bind. Choose [Svenska](#svenska) or [English](#english).

---

## Svenska

### Innan du börjar

Anslut helst ett USB-minne eller montera en nätverkskatalog för backupen. Den
verifierade `.ftwbak`-filen måste ligga utanför den aktiva `data/`-katalogen.
Utan `--backup-dir` används `<installationen>/ftw-backups`, vilket skyddar mot
en felaktig migrering men inte mot att hela SD-kortet går sönder.

Kör från installationskatalogen:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/migrate-legacy-compose.sh \
  -o /tmp/ftw-migrate.sh
RELEASE=v1.4.1-beta.1 # ersätt med den verifierade release som operatören anger
bash /tmp/ftw-migrate.sh \
  --version "$RELEASE" \
  --dir "$PWD" \
  --backup-dir /media/$USER/FTW-BACKUP
```

Om du saknar extern disk kan du utelämna `--backup-dir`, men kopiera den
utskrivna `.ftwbak`-filen från `ftw-backups/` till en annan dator direkt efter
migreringen. Skriptet letar annars i aktuell katalog, `~/ftw` och
`~/forty-two-watts`; om flera installationer hittas måste `--dir` anges.

### Fyra oberoende faser

1. **Full backup.** Den nya backuphjälparen öppnar den äldre databasen
   skrivskyddat, gör ingen schemamigrering, bygger en komplett `.ftwbak`,
   verifierar filhashar och SQLite och stoppar vid minsta fel.
2. **Core + updater.** Exakt samma oföränderliga release-tagg används för båda.
   Updatern startas före Core, och det parade kontrollplanet återskapas med
   samma data-bind. Core måste både vara frisk på `/api/health` och helt
   startklar på `/api/status`; annars återställs Compose, tidigare
   oföränderliga image-ID:n och containrar automatiskt.
3. **Optimizer.** Optimizern hämtas och hälsokontrolleras separat. Om den
   misslyckas ligger den friska Core kvar och använder sin säkra Go-fallback;
   tidigare Optimizer återstartas när den finns.
4. **Drivers.** Endast det signerade katalogmanifestet uppdateras. Ingen driver
   installeras, aktiveras eller startas om under migreringen. Senare driverbyte
   sker en driver i taget i Update Center.

Compose-kopior och tidigare image-ID:n sparas dessutom i
`.ftw-migration-backup-<tid>`. De är en snabb distributionsrollback; `.ftwbak`
är den portabla datakopian.

### Verifiera

```bash
docker compose config --images
docker compose ps
curl -fsS http://127.0.0.1:8080/api/health
curl -fsS http://127.0.0.1:8080/api/status
```

Core och updater ska vara igång på `ghcr.io/srcfl/ftw` respektive
`ghcr.io/srcfl/ftw-updater`. Optimizern kan repareras senare utan att Core eller
data rullas tillbaka. Det är normalt att en migrerad installation behåller
katalogen `~/forty-two-watts` och servicenamnet `forty-two-watts`.

Skriptet stoppar hellre än att gissa vid tvetydig layout, saknad updater,
annan `state.path`, icke-beständig `/app/data` eller en override som kräver
manuell sammanslagning. Dela hela felmeddelandet i en
[GitHub issue](https://github.com/srcfl/ftw/issues). Installera inte en tom
kopia ovanpå den gamla och radera inte `data/`.

---

## English

### Before starting

Prefer a mounted USB disk or network share for the backup. The verified
`.ftwbak` must be outside the live `data/` directory. Without `--backup-dir`,
the script uses `<installation>/ftw-backups`; that protects against a bad
migration but not loss of the whole SD card.

Run from the installation directory:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/migrate-legacy-compose.sh \
  -o /tmp/ftw-migrate.sh
RELEASE=v1.4.1-beta.1 # replace with the operator-approved verified release
bash /tmp/ftw-migrate.sh \
  --version "$RELEASE" \
  --dir "$PWD" \
  --backup-dir /media/$USER/FTW-BACKUP
```

If no external disk is available, omit `--backup-dir` and copy the printed
archive from `ftw-backups/` to another computer immediately afterwards. The
script can also discover the current directory, `~/ftw`, or
`~/forty-two-watts`; ambiguous installations require `--dir`.

### Four independent phases

1. **Full backup.** The new helper opens the legacy database read-only, performs
   no schema migration, creates a complete archive, and verifies file hashes
   plus SQLite before any deployment change.
2. **Core + updater.** The same immutable release tag is used for both and the
   updater starts first. The paired control plane is recreated on the same data
   bind and must pass both `/api/health` and full readiness on `/api/status`.
   Failure restores Compose, the prior immutable image IDs, and the previous
   containers automatically.
3. **Optimizer.** Optimizer is pulled and health-checked separately. Failure
   leaves healthy Core online on its safe Go fallback and restores the prior
   Optimizer when possible.
4. **Drivers.** Only signed catalog metadata is refreshed. No driver is
   installed, activated, or restarted during migration; later changes happen
   one driver at a time in Update Center.

Compose copies and prior image IDs are also retained under
`.ftw-migration-backup-<time>`. They are a deployment rollback point; the
`.ftwbak` is the portable data recovery copy.

### Verify

```bash
docker compose config --images
docker compose ps
curl -fsS http://127.0.0.1:8080/api/health
curl -fsS http://127.0.0.1:8080/api/status
```

Core and updater must be running from `ghcr.io/srcfl/ftw` and
`ghcr.io/srcfl/ftw-updater`. Optimizer can be repaired independently without
rolling back Core or persistent data. Keeping a legacy directory or the
`forty-two-watts` service name is intentional.

The script stops instead of guessing for ambiguous layouts, a missing updater,
a custom `state.path`, non-persistent `/app/data`, or an override that needs a
manual merge. Share the complete error in a
[GitHub issue](https://github.com/srcfl/ftw/issues). Do not install an empty
copy over the old one and do not delete `data/`.
