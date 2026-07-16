# Upgrade an older installation to Sourceful FTW

This is the shareable upgrade guide for existing Docker Compose installations
of Forty Two Watts / FTW. It is available in [Swedish](#svenska) and
[English](#english).

The compatibility fix described here requires **FTW v0.128.1 or later** to be
published. Do not run these steps before that release appears on the
[FTW releases page](https://github.com/srcfl/ftw/releases).

---

## Svenska

### Vad guiden gäller

Följ den här guiden om du redan har Forty Two Watts eller FTW installerat med
Docker Compose. Det gäller även äldre installationer som:

- ligger i `~/forty-two-watts` i stället för `~/ftw`;
- fortfarande har Compose-servicen `forty-two-watts`;
- använder en gammal eller lokal hårdkodad image, till exempel
  `forty-two-watts:optimizer-...`;
- visar felet `does not reference FTW_IMAGE_TAG` när du trycker på Update.

Uppgraderingen behåller den befintliga katalogen, Compose-projektet, servicen,
`config.yaml`, databasen, historiken, enhetsidentiteterna och övrig data. Du ska
inte skapa en parallell ny installation.

### Innan du börjar

1. Kontrollera att **v0.128.1 eller senare** finns på
   [GitHub Releases](https://github.com/srcfl/ftw/releases).
2. Anslut till maskinen med SSH eller öppna dess terminal.
3. Gå till den katalog som redan används av installationen:

```bash
if [ -d "$HOME/ftw" ]; then
  cd "$HOME/ftw"
elif [ -d "$HOME/forty-two-watts" ]; then
  cd "$HOME/forty-two-watts"
else
  echo "Hittade varken ~/ftw eller ~/forty-two-watts"
fi
```

Kontrollera att du är på rätt plats:

```bash
docker compose config --services
docker compose ps
```

Du ska se huvudservicen `ftw` eller `forty-two-watts` och servicen
`ftw-updater`. Om `ftw-updater` saknas, avbryt och be om hjälp i stället för
att ersätta Compose-filen manuellt.

### Steg 1: uppdatera endast updatern

Den gamla updatern kan inte laga sig själv innan den har fått den nya koden.
Uppdatera därför sidecar-servicen en gång:

```bash
docker compose pull ftw-updater
docker compose up -d --no-deps ftw-updater
```

Det här startar inte om huvudservicen och ändrar inte data under `/app/data`.

Kontrollera att updatern kör:

```bash
docker compose ps ftw-updater
docker compose logs --tail=50 ftw-updater
```

Status ska vara `Up` eller `running`. Loggen ska inte visa att updatern avslutas
direkt.

### Steg 2: kör den vanliga uppdateringen i gränssnittet

1. Öppna den lokala FTW-sidan. Använd installationens befintliga adress, till
   exempel `http://ftw.local:8080/` eller `http://42w.local:8080/`.
2. Ladda om sidan.
3. Öppna uppdateringsdialogen och tryck **Update** igen.
4. Vänta tills uppdateringen visar att den är klar och sidan laddas om.

Updatern skapar först en snapshot och flyttar sedan huvudservicen till den
begärda, versionslåsta `ghcr.io/srcfl/ftw`-imagen. Om den gamla Compose-filen
har en hårdkodad image används en tillfällig säker override. Filen på värden
skrivs inte om, och det gamla service- och dataupplägget behålls.

### Steg 3: verifiera

Kontrollera versionen i FTW-gränssnittet och kör:

```bash
docker compose ps
docker compose images
```

Huvudservicen ska vara igång och FTW ska rapportera den nya versionen. Det är
normalt att servicen fortfarande heter `forty-two-watts` och att katalogen
fortfarande heter `~/forty-two-watts`; de identiteterna behålls för att skydda
befintlig data och drift.

### Om `FTW_IMAGE_TAG`-felet fortfarande visas

Tvinga fram en ny updater-container och försök sedan Update i gränssnittet
igen:

```bash
docker compose pull ftw-updater
docker compose up -d --no-deps --force-recreate ftw-updater
docker compose logs --tail=100 ftw-updater
```

Om felet kvarstår, dela utdata från följande kommandon i en
[GitHub issue](https://github.com/srcfl/ftw/issues) eller projektets Discord.
Ta bort eventuella hemligheter innan du delar utdata.

```bash
docker compose config --services
docker compose ps
docker compose images
docker compose logs --tail=100 ftw-updater
```

### Gör inte detta

- Kör inte installationsskriptet i en ny katalog för att “migrera”.
- Byt inte namn på den befintliga katalogen, servicen eller Compose-projektet.
- Kopiera eller radera inte `data/`, `config.yaml` eller SQLite-filerna.
- Ersätt inte en anpassad `docker-compose.yml` med en ny fil utan granskning.
- Kör inte bara `docker compose pull` för en hårdkodad main-image; en
  hårdkodad tagg kan då inte byta version.

---

## English

### Who should use this guide

Follow this guide if Forty Two Watts or FTW is already installed with Docker
Compose. It also covers older installations that:

- live in `~/forty-two-watts` instead of `~/ftw`;
- still use the Compose service name `forty-two-watts`;
- use an old or locally hard-coded image such as
  `forty-two-watts:optimizer-...`;
- show `does not reference FTW_IMAGE_TAG` when Update is selected.

The upgrade preserves the existing directory, Compose project, service,
`config.yaml`, database, history, device identities, and other data. Do not
create a parallel fresh installation.

### Before you start

1. Confirm that **v0.128.1 or later** is available on
   [GitHub Releases](https://github.com/srcfl/ftw/releases).
2. Connect to the host over SSH or open its terminal.
3. Enter the directory already used by the installation:

```bash
if [ -d "$HOME/ftw" ]; then
  cd "$HOME/ftw"
elif [ -d "$HOME/forty-two-watts" ]; then
  cd "$HOME/forty-two-watts"
else
  echo "Neither ~/ftw nor ~/forty-two-watts was found"
fi
```

Confirm that this is the correct directory:

```bash
docker compose config --services
docker compose ps
```

You should see the main service, named either `ftw` or `forty-two-watts`, and
the `ftw-updater` service. If `ftw-updater` is missing, stop and ask for help
instead of manually replacing the Compose file.

### Step 1: refresh only the updater

The old updater cannot acquire this compatibility fix until its own container
has been refreshed once:

```bash
docker compose pull ftw-updater
docker compose up -d --no-deps ftw-updater
```

This does not restart the main service or change anything under `/app/data`.

Confirm that the updater is running:

```bash
docker compose ps ftw-updater
docker compose logs --tail=50 ftw-updater
```

Its status should be `Up` or `running`, and the log should not show the updater
exiting immediately.

### Step 2: run the normal update in the UI

1. Open the existing local FTW address, for example
   `http://ftw.local:8080/` or `http://42w.local:8080/`.
2. Reload the page.
3. Open the update dialog and select **Update** again.
4. Wait for the update to complete and the page to reload.

The updater first creates a snapshot, then moves the main service to the
requested immutable `ghcr.io/srcfl/ftw` image. If the old Compose file contains
a hard-coded image, the updater adds a temporary safe override. It does not
rewrite the host file, and it preserves the existing service and data layout.

### Step 3: verify

Check the version in the FTW UI, then run:

```bash
docker compose ps
docker compose images
```

The main service should be running and FTW should report the new version. It is
normal for the service to remain named `forty-two-watts` and for the directory
to remain `~/forty-two-watts`; these identities are preserved to protect
existing data and operation.

### If the `FTW_IMAGE_TAG` error still appears

Force a fresh updater container, then retry Update in the UI:

```bash
docker compose pull ftw-updater
docker compose up -d --no-deps --force-recreate ftw-updater
docker compose logs --tail=100 ftw-updater
```

If it still fails, share the output of the following commands in a
[GitHub issue](https://github.com/srcfl/ftw/issues) or the project Discord.
Remove any secrets before sharing the output.

```bash
docker compose config --services
docker compose ps
docker compose images
docker compose logs --tail=100 ftw-updater
```

### Do not do this

- Do not run the installer into a new directory as a migration method.
- Do not rename the existing directory, service, or Compose project.
- Do not copy or delete `data/`, `config.yaml`, or the SQLite files.
- Do not replace a customized `docker-compose.yml` without reviewing it.
- Do not rely on `docker compose pull` alone when the main image tag is
  hard-coded; a hard-coded tag cannot select a new version.
