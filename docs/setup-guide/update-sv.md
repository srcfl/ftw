# Byt till nya FTW och håll det uppdaterat

**Detta utkast gäller `v0.142.1`; stable är ännu inte publicerad.** FTW
2.x och 3.x får inga fler uppdateringar. [English](../native-beta.md).

## Kort svar

| Du kör | Gör så här |
|---|---|
| Nya FTW 0.x, native | Öppna **More → Version → Update FTW** i webbsidan, eller kör `ftw update` på boxen. Inget mer. |
| Nya FTW 0.x i Docker | Sätt `FTW_VERSION=v0.142.1` i `.env` och kör `docker compose up -d --build`. [Detaljer](#docker-från-det-nya-releasepaketet) |
| Inget FTW än, på 64-bitars Linux eller ett nytt Pi-kort | [Installera](#installera-på-det-nya-kortet-eller-en-tom-linux-maskin) med den stabila taggen. |
| Gamla FTW 2.x, 3.x eller den gamla Pi-imagen | Flytta data och config med en full backup till ett andra SD-kort eller en annan maskin. [Steg](#byt-på-raspberry-pi-med-ett-nytt-sd-kort) |
| Home Assistant-appen | Ingen ny app än. Kör nya FTW på en annan Linux-maskin och koppla Home Assistant via MQTT. [Steg](#välj-en-väg) |
| Windows eller macOS | Stöds inte. Använd en Raspberry Pi eller en annan 64-bitars Linux-maskin. |

Låt boxen ha ström och tryck inte Restart medan uppdateringen pågår. Vänta tills
uppdateringen är klar. På native återgår `ftw rollback` till föregående
kompatibla release. För Docker, följ [Docker-steget](#docker-från-det-nya-releasepaketet).

**Ta med inställningar, historik, identitet och mål.** Börja med
[Flytta med din data](../migration.md#svenska). Ta en full backup, importera
på en separat kopia och kontrollera resultatet innan nya FTW tar över.
Behåll gamla installationen och en backup utanför maskinen för återgång.
En befintlig anläggning använder sina sparade inställningar.

Guiden anger provade datafall och kvarvarande arbete för andra layoutar.
Vid ett importfel: behåll båda kopiorna och lös felet före start. Maskerad
API-config är ingen full export. DuckDB använder
[målversionens konverterare](../history-conversion.md).

Den nya serien börjar med `v0.131.0-beta.1`.
Äldre 0.x-versioner, till och med 0.130.x, hör till den gamla serien. `beta` i
en gammal installation flyttar den inte till nya serien, och 3.x-beta är inget
steg på vägen.

## Välj en väg

**Sitter det gamla FTW-kortet fortfarande i Pi:n? Kör ingen nyinstallation där.**
Vägen för dig som vill ha hjälp steg för steg är ett **andra SD-kort**.
Du ska byta det fysiska kortet innan du kör installationskommandot.

Välj **en** av följande vägar. De är alternativ, inte steg efter varandra:

- **Gammal FTW på Raspberry Pi:** [byt med ett nytt SD-kort](#byt-på-raspberry-pi-med-ett-nytt-sd-kort).
  Det gamla kortet och dess data blir kvar.
- **Redan nya 0.x:** [uppdatera din befintliga installation](#uppdatera-när-du-redan-kör-nya-ftw).
  Kör ingen nyinstallation.
- **Tom Linux-maskin utan FTW:** [installera native](#installera-på-det-nya-kortet-eller-en-tom-linux-maskin).
  Docker är ett eget alternativ för dig som kan hantera Docker.

<details>
<summary>Andra installationer och hela tabellen med vägval</summary>

| Det du har nu | Vägen till nya FTW | Nästa uppdatering |
|---|---|---|
| Äldre Docker: gammal 0.x, 1.x eller 2.x, även äldre Forty Two Watts-images | Nytt SD-kort/annan Linux-värd, eller separat Docker-installation på samma Linux-värd. Stoppa gamla Core innan nya Core får styra. | Följ metoden för den nya installationen nedan. |
| Docker 3.x, inklusive 3.x-beta | Samma väg som från äldre Docker. Ingen mellanversion behövs. | Följ metoden för den nya installationen. |
| SD-kort med den gamla färdiga FTW-imagen | Använd ett nytt kort med Raspberry Pi OS Lite 64-bit. Behåll det gamla kortet. Den gamla imagen innehåller Docker; den är ingen egen uppdateringskanal. | `ftw update` på den nya native-installationen. |
| Nya 0.x med native launcher och systemd | Uppdatera den befintliga installationen. Kör inte en nyinstallation. | `ftw update`, eller `ftw update --channel beta` för att välja beta. |
| Nya 0.x i Docker, byggd från releasepaketet | Behåll Compose-projektet och dess datakatalog. | Ändra `FTW_VERSION` i `.env`, kör `docker compose up -d --build`. |
| Äldre native-installation utan release slots | Identifiera tjänsten och datavägarna. Ny installation på separat värd/kort, eller en särskilt kontrollerad flytt. | Det befintliga `migrate-native`-verktyget är en pilot, inte en allmän migrationsguide. |
| Home Assistant-app på gamla serien | Installera nya FTW på en separat Linux-värd och stoppa gamla appens Core, Start on boot och Watchdog innan du tar över. Installera inte 3.x-beta för att få det senaste. | Supervisor hanterar den gamla appen. Den ger ännu ingen väg till nya 0.x. |
| Egen build, manuellt startad binär, macOS eller Windows | Identifiera körsättet först. Den dokumenterade nya installationen kräver 64-bitars Linux. | Använd inte Linux-kommandon som om de gällde varje installation. |

`ftw.service` eller en fungerande `ftw status` räcker inte för att skilja
körsätten åt. Samma tjänstenamn har använts tidigare, och status visar den Core
som svarar på adressen.

</details>

## Om du redan har fått ett fel

**Stanna vid första felet. Kör inte nästa block och byt inte metod för att
komma förbi felet.** Spara kommandot och svaret. Be om hjälp i
[Discord](https://discord.gg/UK2ygPBu8N) om nästa steg är oklart.

| Det du ser | Gör så här |
|---|---|
| `Existing FTW installation found` eller nekad `--fresh-host` | Du kör på en maskin eller ett kort med FTW-data. Behåll filerna. På Pi: stäng av och byt till det förberedda nya kortet. Äldre publicerade skript kan säga “try 0.x beside it”; det betyder inte att Docker är nästa steg. |
| `curl: (22)` eller `404` | Nedladdningen misslyckades. Kopiera den exakta taggen från Releases och kontrollera länken. Kör inte en gammal `install.sh` eller kvarlämnade Docker-filer. |
| `ftw-local` finns redan | Ett tidigare försök har lämnat filer. Kontrollera projektet innan du gör mer. Radera inte data och skriv inte över `.env`. |
| `requires buildx plugin` | Docker saknar ett byggverktyg. Ordna förkraven i Docker-guiden innan du bygger. |
| `checking context: no permission to read .../data/applink.json` | Docker försöker läsa privata FTW-data när det bygger. Behåll filernas rättigheter. Se steget med `.dockerignore` under [Docker-uppdatering](#docker-från-det-nya-releasepaketet). Lös inte felet med `chmod` eller genom att bygga som root. |
| Nya `ftw-local` visar `Restarting` medan gamla Core visar `Up` | Stoppa **den nya testcontainern** med dess kontrollerade namn: `sudo docker stop --time 60 <nya-containerns-namn>`. Behåll båda installationernas data och felsök innan du startar den igen. Stoppa inte en delad MQTT-broker. |

## Börja här om du inte vet vad som körs

Öppna en terminal på FTW-maskinen, eller anslut med SSH. Följande kommandon
läser bara tillståndet:

```bash
sudo docker ps -a --format 'table {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}'
sudo docker compose ls -a
systemctl list-units --type=service --all --no-pager | grep -Ei 'ftw|forty|docker'
systemctl list-unit-files --type=service --no-pager | grep -Ei 'ftw|forty'
```

Spara också versionsnumret och adressen till webbsidan du öppnar. Om ett
kommando saknas eller ger ett fel, spara felet. Det bevisar inte att en gammal
installation saknas. Har du egna startskript eller kör FTW på flera maskiner
behöver även de kontrolleras.

En Docker-image på disken är inte en körande FTW. En stoppad container är inte
heller aktiv. Kontrollera vilken Core som faktiskt körs och vad som kan starta
den igen vid omboot.

## Byt på Raspberry Pi med ett nytt SD-kort

1. **På gamla kortet:** spara enheternas adresser, mål och scheman. Ta en full
   backup enligt den installerade versionens instruktioner och spara den
   utanför Pi:n. Kör inget installationskommando på detta kort.
2. **På din vanliga dator:** skriv Raspberry Pi OS Lite **64-bit** till ett
   **andra** kort. Följ [Pi-guidens steg 1–7](sv.md#steg-1--hämta-programmet-som-förbereder-minneskortet).
   Välj ett användarnamn som inte är `ftw`, till exempel `pi`, och slå på SSH.
   Skriv inte över det gamla kortet.
3. **Vid Pi:n:** stäng av, dra ur strömmen, ta ut gamla kortet och sätt i det
   nya. Anslut strömmen igen. **Det fysiska kortbytet ska vara klart innan du
   kör någon installation.**
4. **På din vanliga dator:** öppna en ny SSH-anslutning till Pi:n med det
   användarnamn du valde för nya kortet. IP-adressen kan ha ändrats; kontrollera
   routern. Har du inte bytt kort, stanna här.
5. **Innan du ställer in enheterna:** stoppa gammal FTW på andra maskiner
   som kan styra samma utrustning. Ordna MQTT om utrustningen behöver det.
   En broker på gamla kortet följer inte med.
6. **I SSH på nya kortet:** följ [Installera på det nya kortet](#installera-på-det-nya-kortet-eller-en-tom-linux-maskin)
   nedan. [Importera din backup](../migration.md#restore-a-full-backup-on-a-new-native-target)
   innan nya Core tar över. Kontrollera sparade mål och inställningar. Den
   sparade API-porten kan skilja sig från 8080. Använd mål och ready-by-scheman
   för tidigare kalenderhändelser.
7. Kontrollera rätt version, friska enheter, färska mätvärden och aktuell plan.
   Starta sedan om Pi:n när anläggningen kan tåla avbrottet och kontrollera
   samma saker igen.

För att gå tillbaka: stäng av Pi:n och sätt tillbaka det gamla kortet. Stoppa
också eventuell ny FTW på en annan maskin innan gamla FTW får styra igen. Data
som den nya installationen har samlat ligger kvar på det nya kortet.

## Byt på samma Linux-maskin med Docker

**Detta är ett avancerat alternativ till kortbytet ovan.** Välj det bara om
du kan identifiera och stoppa gamla Core och updater, hantera deras startregler
och bevara MQTT. Välj annars nytt kort eller be om hjälp. En nekad
native-installation på gamla kortet är inte ett skäl att fortsätta med Docker.

Den nya Docker-installationen använder en egen katalog.
[Importera en verifierad kopia](../migration.md#svenska) av dina data där.

1. Identifiera det gamla Compose-projektet, dess Core, updater, datakataloger
   och startregler. Vanliga kataloger är `/opt/ftw`, `~/ftw` och
   `~/forty-two-watts`, men använd den väg som finns på maskinen.
2. Ta en full backup och spara den utanför maskinen. Behåll också Compose-filer,
   overrides och `.env`, så att du kan återställa rätt gamla version.
3. Kontrollera om projektet även kör Mosquitto eller andra tjänster som
   utrustningen behöver. Behåll dem igång. Identifiera tjänsterna med
   `docker compose config --services`, projektets riktiga filer och projektnamn.
4. Stoppa bara gamla Core och updater. Heter Core-tjänsten `ftw`, kör
   `docker compose stop ftw`, sedan
   `docker update --restart=no "$(docker compose ps -aq ftw)"`. Gör samma sak
   för updater om den finns. Sätt också `restart: "no"` för dessa tjänster i
   gamla Compose-filerna. Kontrollera att containrarna är stoppade och har
   restart policy `no`. Kör inte `docker compose down` om projektet fortfarande
   ger utrustningen MQTT. Använd inte `-v` och radera inga datakataloger.
   Kontrollera även systemd, timers och egna skript som kan skapa eller starta
   den igen. Ange brokerns tjänstenamn i senare Compose-kommandon:
   `docker compose up -d` för hela gamla projektet startar gamla Core igen,
   även med `restart: "no"`. Stäng inte av Docker som helhet.
5. Följ [Docker-guiden för nya FTW](https://github.com/srcfl/ftw/blob/master/docs/native-beta.md#docker).
   Använd en ny katalog, normalt `~/ftw-local`, och en tom datakatalog.
   Återanvänd inte den gamla datakatalogen eller en redan befintlig testkatalog
   utan att först kontrollera vad den innehåller.
6. Importera din backup med nya Core stoppad. Kontrollera sparade inställningar,
   historik, version, enheter, mätvärden och plan innan den tar över.
   Kontrollera att bara en Core körs och styr utrustningen. Upprepa kontrollen
   efter en planerad omboot.

För att gå tillbaka: stoppa nya FTW först. Starta sedan det bevarade gamla
projektet med dess sparade filer och versionsval. Återaktivera bara de
startregler du själv stängde av. Den gamla och nya installationen behåller var
sin data.

**Port 8080 är ingen spärr mot dubbel styrning.** Äldre Core kan fortsätta
styra även om den inte kan starta sin webbsida. En enda synlig webbsida är
därför inget bevis på att bara en FTW körs.

## Installera på det nya kortet eller en tom Linux-maskin

**Bara på nya kortet eller en tom värd.** Har gamla FTW körts på det här
kortet, gå tillbaka till kortbytet ovan. `--fresh-host` betyder att ingen
FTW-installation eller dess data finns här, även om den är stoppad.

1. Öppna [Releases](https://github.com/srcfl/ftw/releases) på din vanliga dator.
   Välj den senaste **stabila** taggen, `v0.142.1` eller senare utan `-beta`.
   Välj en beta bara om du provar nya byggen. Releasen ska ha Linux-paket och
   SHA-256-fil för din maskin. Använd inte `releases/latest`; den pekar på
   gamla 2.x.
2. Kopiera taggen från releasens rubrik. Skriv inte av den för hand. Byt bara
   ut `v0.X.Y` i blocket nedan mot taggen du kopierade.
3. Klistra in **hela blocket, inklusive `(` och `)`**, i SSH på nya kortet.
   Det stannar vid första felet och hämtar skriptet till en tillfällig fil.

```bash
(
  set -eu
  tag=v0.X.Y
  if [[ ! "$tag" =~ ^v0\.([0-9]+)\.[0-9]+(-beta\.[0-9]+)?$ ]] || (( 10#${BASH_REMATCH[1]} < 131 )); then
    echo "STOP: copy an exact published new 0.x tag from Releases." >&2
    exit 1
  fi
  installer=$(mktemp)
  trap 'rm -f "$installer"' EXIT
  curl -fSL "https://raw.githubusercontent.com/srcfl/ftw/${tag}/scripts/install.sh" -o "$installer"
  bash "$installer" --fresh-host --tag "$tag"
)
```

Om installationen lyckas, öppna `http://<Pi:ns-IP>:8080/setup` och ställ in
anläggningen. Om den visar ett fel, stanna och läs [feltabellen](#om-du-redan-har-fått-ett-fel).
Fortsätt inte med Docker-kommandon.

## Uppdatera när du redan kör nya FTW

### Native med launcher

Kör på FTW-maskinen:

```bash
ftw status
ftw update
ftw status
journalctl -u ftw --since "10 minutes ago" --no-pager
```

`ftw update` följer kanalen du valt med `--channel`. Har du aldrig valt följer
den det boxen kör: en stabil version följer stable, en beta följer beta. Beta
erbjuder också en nyare stable, så en betabox går över till stable när en
sådan kommer. Vill du stanna på beta efter det, kör `ftw update --channel beta`;
vill du lämna beta, kör `ftw update --channel stable`. Du kan också välja
**More → Version → Update FTW** i native-webbsidan. Knappen använder samma
Core-API och launcher som kommandot. Den visar hämtade byte, klara steg och
resultatet efter omstart. Behåll strömmen på. Att stänga eller ladda om sidan
avbryter inte uppdateringen och startar inte ett nytt försök.

Kontrollera vilken version som faktiskt körs efteråt. En publicerad release
eller ett lyckat kommando som säger att installationen redan är aktuell är
inte bevis på att just den version du avsåg installerades.

`ftw rollback` återgår till föregående kompatibla release med nuvarande data.
Det ersätter ingen full backup. En ändrad dataversion kan kräva en annan
återställningsväg. En native-installation kan också falla tillbaka automatiskt
om en ny release inte startar eller fortsätter krascha; spara `ftw status` och
loggar innan du försöker igen.

Kör inte om nyinstallationen för en vanlig uppdatering. `--refresh` gäller
launcher, CLI och tjänstefil och används när releasen kräver det. En äldre
pilotinstallation med annan katalogstruktur behöver sina egna kontroller.

### Docker från det nya releasepaketet

Detta gäller bara **nya 0.x**, inte gamla 2.x/3.x. Gå till det kontrollerade
nya projektets katalog, normalt `cd ~/ftw-local`. Om du installerade med den
tidigare guiden, skapa den saknade `.dockerignore` bredvid `Dockerfile` först:

```bash
if [ ! -e .dockerignore ]; then
  printf '*\n!Dockerfile\n!.dockerignore\n' > .dockerignore
fi
```

Om filen redan finns, kontrollera att den utesluter `data` och `.env` från
bygget. Behåll datafilernas rättigheter.

Öppna `.env` i det verifierade nya Compose-projektet och ändra `FTW_VERSION`
till den exakta publicerade release du vill installera. Behåll övriga rader.
Kör sedan i samma projekt:

```bash
docker compose up -d --build
docker compose ps
docker compose exec ftw ftw status
```

Kontrollera även mätvärden och plan. `ftw update` och `ftw rollback` gäller
inte den här Docker-installationen. Återgång sker genom att välja föregående
kompatibla `FTW_VERSION` och köra Compose igen. Docker har ingen automatisk
återgång till föregående version.

### Backup och restore för nya Docker

Använd `ftw-backup` från samma release-image. Verktyget verifierar ett
portabelt `.ftwbak`-arkiv och behåller tidigare data vid restore. Stegen gäller
det nya Compose-projektet, inte flytt av en äldre installation. Kör från
`~/ftw-local`. Bara tjänsten `ftw` stoppas.

Montera en annan disk först. Byt `/media/usb/ftw-backups` till en särskild
backupkatalog där. Containern använder UID 100 och GID 101; blocket skapar
katalogen med dessa ägare och privata rättigheter:

```bash
(
  set -eu
  backup_dir=/media/usb/ftw-backups
  sudo install -d -o 100 -g 101 -m 0700 "$backup_dir"
  docker compose stop ftw
  docker compose run --rm --no-deps --entrypoint /app/ftw-backup \
    --volume "$backup_dir:/backup:z" ftw \
    create -state /app/data/state.db -data /app/data -output /backup -progress
  docker compose start ftw
)
```

Slutlig JSON anger det verifierade arkivets namn under `/backup`. Filen finns
i `backup_dir` på värden. Spara namnet. Vid fel stannar blocket och FTW förblir
stoppad. Lös felet innan du startar igen; en ofärdig fil är ingen backup.

För restore: ange exakt `.ftwbak`-namn och använd en kompatibel release-image:

```bash
(
  set -eu
  backup_dir=/media/usb/ftw-backups
  archive=ftw-full-backup-YYYYMMDDTHHMMSS.ftwbak
  test -f "$backup_dir/$archive"
  docker compose stop ftw
  docker compose run --rm --no-deps --entrypoint /app/ftw-backup \
    --volume "$backup_dir:/backup:ro,z" ftw \
    restore -archive "/backup/$archive" -data /app/data -yes
  docker compose start ftw
)
```

Verktyget verifierar och packar upp innan data ersätts. Ett fel stoppar blocket
innan Core startar. Tidigare data finns kvar i den utskrivna sökvägen
`/app/data/.ftw-pre-restore-…`. Behåll dem tills config, identitet, gammal och ny
historik, mål, färska mätvärden och plan har kontrollerats. Vänta på frisk
status i `docker compose ps` och kör sedan
`docker compose exec ftw ftw status`.

För att återgå till sparade data: sätt `safety` till den utskrivna sökvägen i
containern. Använd en image som kan läsa dessa data:

```bash
(
  set -eu
  safety=/app/data/.ftw-pre-restore-YYYYMMDDTHHMMSS
  docker compose stop ftw
  docker compose run --rm --no-deps --entrypoint /app/ftw-backup ftw \
    revert -data /app/data -safety "$safety" -yes
  docker compose start ftw
)
```

Verktyget läser `.ftwbak`, inte äldre råa `.tar.gz`-arkiv. Behåll sådana arkiv
och den matchande gamla installationen för återställning. Att döpa om filen
gör den inte kompatibel. Läs [full backup och restore](../backup-and-restore.md).

## För agenter som hjälper till

- Välj en väg. Native och Docker är alternativ, inte steg efter varandra.
  Stanna vid nedladdningsfel eller nekad installation. Använd inte gamla
  nedladdade filer och byt inte metod för att komma förbi spärren.
- Följ samma vägval som människan. Fastställ värd, version, körsätt,
  tjänst/projekt, datavägar, startregler och målversion innan du ändrar något.
- Skilj på uppdatering inom nya serien, nyinstallation med nya data och
  migrering med bevarade data. Säg vilken åtgärd du avser.
- Välj ingen 2.x- eller 3.x-release. Tolka inte ett gammalt erbjudande i
  Update Center eller `releases/latest` som en rekommendation.
- Kör ingen installation, uppdatering eller omstart när uppdraget bara är
  felsökning. En begäran att uppdatera ger rätt att utföra den valda vägen;
  fråga inte igen om samma sak.
- För en befintlig anläggning: verifiera backup och kopian utanför maskinen
  innan ett byte som berör installation eller data. Anta inte att alla gamla
  versioner har samma backupknapp eller datalayout.
- Starta inte nya Core mot samma utrustning medan gamla Core fortfarande körs.
  Kontrollera även andra värdar och Home Assistant. Bevara beroenden som MQTT.
- En misslyckad hälsokontroll ger ingen rätt att radera data, kringgå
  installationsspärrar, köra `down -v` eller prova en annan gammal migration.
- Rapportera målversion och verklig version, Core-processer, startregler,
  hälsa, färska mätvärden, plan, backup och möjlig återgång var för sig.
  Redovisa om kontroll efter omboot eller fysisk styrning återstår.
