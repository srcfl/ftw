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

Skriptet använder installationen i den aktuella katalogen, eller letar i
`~/ftw` och `~/forty-two-watts`. Om båda hemkatalogerna innehåller en
installation stoppar det och kräver `--dir` i stället för att gissa.

Om installationen ligger någon annanstans:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/migrate-legacy-compose.sh -o /tmp/ftw-migrate.sh && bash /tmp/ftw-migrate.sh --dir /sökväg/till/installationen
```

### Vad skriptet gör

Skriptet:

1. kontrollerar att exakt en huvudservice (`ftw` eller `forty-two-watts`),
   `ftw-updater` och den beständiga `/app/data`-mounten finns;
2. säkerhetskopierar Compose-filerna och sparar oföränderliga image-ID:n för
   befintliga berörda containrar innan någon tagg hämtas;
3. behåller befintliga servicefält men ersätter image-referenserna med
   `ghcr.io/srcfl/ftw`, `ghcr.io/srcfl/ftw-updater` och
   `ghcr.io/srcfl/ftw-optimizer`; om optimizer-servicen saknas läggs dess
   socket, volym och service i en separat Compose-override;
4. validerar den sammanslagna Compose-konfigurationen och hämtar de officiella
   multi-arch-imagerna;
5. återskapar huvud-, updater- och optimizercontainrarna med samma katalog,
   Compose-projekt, servicenamn, befintliga miljöinställningar, volymer och
   data-bind; andra tjänster lämnas orörda;
6. kontrollerar att alla tre containrarna och FTW:s lokala health-endpoint svarar.

Om något steg misslyckas återställs tidigare Compose-filer och image-referenser.
Befintliga berörda containrar återskapas; en optimizer som skriptet nyss lade
till tas bort igen.
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
ghcr.io/srcfl/ftw-optimizer:
```

Det är normalt att en migrerad installation fortfarande ligger i
`~/forty-two-watts` och har servicenamnet `forty-two-watts`. De identiteterna
behålls avsiktligt så att ingen parallell tom installation skapas.

### Om skriptet stoppar

Skriptet stoppar hellre än att gissa om layouten är tvetydig, båda
standardkatalogerna finns, updater saknas, `/app/data` inte är en befintlig
host-bind, eller en befintlig override måste slås ihop manuellt. Dela då hela
felmeddelandet i en
[GitHub issue](https://github.com/srcfl/ftw/issues) eller projektets Discord.
Kör inte en ny installation ovanpå den gamla och radera inte `data/`.

---

## English

### Upgrade

Connect to the host over SSH or open its terminal, then run:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/migrate-legacy-compose.sh -o /tmp/ftw-migrate.sh && bash /tmp/ftw-migrate.sh
```

The script uses an installation in the current directory, or searches
`~/ftw` and `~/forty-two-watts`. If both home-directory locations contain an
installation, it stops and requires `--dir` instead of guessing.

For an installation stored elsewhere:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/migrate-legacy-compose.sh -o /tmp/ftw-migrate.sh && bash /tmp/ftw-migrate.sh --dir /path/to/the/installation
```

### What the script does

The script:

1. verifies that exactly one main service (`ftw` or `forty-two-watts`),
   `ftw-updater`, and the persistent `/app/data` mount exist;
2. backs up the Compose files and records immutable image IDs for existing
   affected containers before pulling any tag;
3. preserves existing service fields while replacing the image references
   with `ghcr.io/srcfl/ftw`, `ghcr.io/srcfl/ftw-updater`, and
   `ghcr.io/srcfl/ftw-optimizer`; if the optimizer service is missing, its
   socket, volume, and service are added in a separate Compose override;
4. validates the merged Compose configuration and pulls the official
   multi-architecture images;
5. recreates the main, updater, and optimizer containers with the same
   directory, Compose project, service name, existing environment, volumes,
   and data bind; unrelated services are left untouched;
6. verifies all three containers and the local FTW health endpoint.

If a step fails, the previous Compose files and image references are restored.
Existing affected containers are recreated; an optimizer just added by the
script is removed again.
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
ghcr.io/srcfl/ftw-optimizer:
```

It is normal for a migrated installation to remain in `~/forty-two-watts` and
keep the `forty-two-watts` service name. Those identities are deliberately
preserved so the migration cannot create a parallel empty installation.

### If the script stops

The script stops instead of guessing when a layout is ambiguous, both default
directories exist, the updater is missing, `/app/data` is not an existing host
bind, or an existing override needs a manual merge. Share the complete error
in a
[GitHub issue](https://github.com/srcfl/ftw/issues) or the project Discord. Do
not install a second copy over the old one, and do not delete `data/`.
