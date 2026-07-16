# Upgrade an older installation to Sourceful FTW

Use this guide for an existing Linux Docker Compose installation of Forty Two
Watts or FTW that has a `docker-compose.yml` file. Choose
[Svenska](#svenska) or [English](#english).

The migration keeps the existing directory, Compose project, service name,
configuration, database, history, device identities, and `data/` directory. It
also creates rollback backups before changing the running service.

---

## Svenska

### Uppgradera

Anslut till maskinen med SSH eller öppna dess terminal. Kör sedan:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/migrate-legacy-compose.sh -o /tmp/ftw-migrate.sh && bash /tmp/ftw-migrate.sh
```

Klart. Skriptet hittar automatiskt en befintlig installation i den aktuella
katalogen, `~/ftw` eller `~/forty-two-watts`.

Om installationen ligger någon annanstans:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/migrate-legacy-compose.sh -o /tmp/ftw-migrate.sh && bash /tmp/ftw-migrate.sh --dir /sökväg/till/installationen
```

### Vad skriptet gör

Skriptet:

1. kontrollerar att exakt en huvudservice (`ftw` eller `forty-two-watts`),
   `ftw-updater` och den beständiga `/app/data`-mounten finns;
2. säkerhetskopierar de aktiva Compose-filerna;
3. ändrar endast service-image till `ghcr.io/srcfl/ftw` och
   `ghcr.io/srcfl/ftw-updater`;
4. hämtar de officiella multi-arch-imagerna;
5. återskapar tjänsterna med samma katalog, Compose-projekt, servicenamn,
   miljöinställningar, volymer och data-bind;
6. kontrollerar att båda containrarna och FTW:s lokala health-endpoint svarar.

Om något steg misslyckas återställs tidigare Compose-filer och container.
`data/` kopieras, flyttas eller raderas aldrig.

### Verifiera

Kör i installationskatalogen:

```bash
docker compose config --images
docker compose ps
```

Du ska se images som börjar med:

```text
ghcr.io/srcfl/ftw:
ghcr.io/srcfl/ftw-updater:
```

Det är normalt att en migrerad installation fortfarande ligger i
`~/forty-two-watts` och har servicenamnet `forty-two-watts`. De identiteterna
behålls avsiktligt så att ingen parallell tom installation skapas.

### Om skriptet stoppar

Skriptet stoppar hellre än att gissa om layouten är tvetydig, saknar updater,
eller använder en oväntad data-mount. Dela då hela felmeddelandet i en
[GitHub issue](https://github.com/srcfl/ftw/issues) eller projektets Discord.
Kör inte en ny installation ovanpå den gamla och radera inte `data/`.

---

## English

### Upgrade

Connect to the host over SSH or open its terminal, then run:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/migrate-legacy-compose.sh -o /tmp/ftw-migrate.sh && bash /tmp/ftw-migrate.sh
```

That is all. The script automatically finds an existing installation in the
current directory, `~/ftw`, or `~/forty-two-watts`.

For an installation stored elsewhere:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/migrate-legacy-compose.sh -o /tmp/ftw-migrate.sh && bash /tmp/ftw-migrate.sh --dir /path/to/the/installation
```

### What the script does

The script:

1. verifies that exactly one main service (`ftw` or `forty-two-watts`),
   `ftw-updater`, and the persistent `/app/data` mount exist;
2. backs up the active Compose files;
3. changes only the service images to `ghcr.io/srcfl/ftw` and
   `ghcr.io/srcfl/ftw-updater`;
4. pulls the official multi-architecture images;
5. recreates the services with the same directory, Compose project, service
   name, environment, volumes, and data bind;
6. verifies both containers and the local FTW health endpoint.

If a step fails, the previous Compose files and container are restored.
`data/` is never copied, moved, or deleted.

### Verify

Run from the installation directory:

```bash
docker compose config --images
docker compose ps
```

The output should include images beginning with:

```text
ghcr.io/srcfl/ftw:
ghcr.io/srcfl/ftw-updater:
```

It is normal for a migrated installation to remain in `~/forty-two-watts` and
keep the `forty-two-watts` service name. Those identities are deliberately
preserved so the migration cannot create a parallel empty installation.

### If the script stops

The script stops instead of guessing when a layout is ambiguous, lacks the
updater, or uses an unexpected data mount. Share the complete error in a
[GitHub issue](https://github.com/srcfl/ftw/issues) or the project Discord. Do
not install a second copy over the old one, and do not delete `data/`.
