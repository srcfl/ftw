# Byt till nya FTW och håll det uppdaterat

[English and release commands](../native-beta.md).

**FTW 2.x och 3.x får inga fler uppdateringar. All fortsatt utveckling går
till den nya 0.x-serien. Vill du ha de senaste funktionerna och rättningarna
är det dags att byta nu. Installera inte 3.x-beta, och uppdatera inte en äldre
installation till 3.x som ett steg på vägen.**

Den nya serien börjar med `v0.131.0-beta.1`. Det lägre versionsnumret är
avsiktligt. Äldre 0.x-versioner, till och med 0.130.x, hör till den gamla
serien. `beta` är kanalen inom en serie; ett kanalbyte flyttar inte en gammal
installation till den nya serien.

Du kan börja använda nya FTW nu, med nya inställningar och egen datalagring.
Den guidade flytten som tar med gamla inställningar och historik är ännu inte
klar. Behöver du föra över dessa data innan du byter, ta hjälp med just din
installation. Kopiera inte gamla databaser till den nya installationen på egen
hand.

## Välj väg efter hur FTW körs

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

1. Spara uppgifter om enheter, adresser, mål och scheman. Ta en full backup
   enligt instruktionerna för den installerade versionen och spara den utanför
   Pi:n. Behåll även det gamla kortet.
2. Skriv Raspberry Pi OS Lite **64-bit** till ett **nytt** kort. Välj ett
   användarnamn som inte är `ftw`, och slå på SSH. Skriv inte över det gamla
   kortet.
3. Stäng av Pi:n, byt kort och starta. Om gamla FTW körs på en annan maskin,
   stoppa den innan nya FTW får styra samma utrustning.
4. Välj en publicerad 0.x-beta från
   [Releases](https://github.com/srcfl/ftw/releases). Den ska ha paketet för
   din arkitektur och dess SHA-256-fil. Använd inte `releases/latest`: den
   länken pekar fortfarande på den gamla serien.
5. Följ [installationsstegen](https://github.com/srcfl/ftw/blob/master/docs/native-beta.md#install)
   med den valda taggen. Installationsskriptet och paketet ska komma från samma
   tagg. `--fresh-host` gäller bara en värd utan en befintlig FTW-installation.
6. Öppna `http://<Pi:ns-IP>:8080/setup` och ställ in anläggningen på nytt.
   Historik, inlärning och gamla inställningar ligger kvar på det gamla kortet;
   de följer inte automatiskt med. Ersätt gamla kalenderhändelser med mål och
   ready-by-scheman. Kontrollera även MQTT: en broker på det gamla kortet
   följer inte med till det nya.
7. Kontrollera rätt version, friska enheter, färska mätvärden och aktuell plan.
   Starta sedan om Pi:n när anläggningen kan tåla avbrottet och kontrollera
   samma saker igen.

För att gå tillbaka: stäng av Pi:n och sätt tillbaka det gamla kortet. Stoppa
också eventuell ny FTW på en annan maskin innan gamla FTW får styra igen. Data
som den nya installationen har samlat ligger kvar på det nya kortet.

## Byt på samma Linux-maskin med Docker

Den nya Docker-installationen använder en egen katalog och egen data. Det är
en ny installation, inte en flytt av gamla data.

1. Identifiera det gamla Compose-projektet, dess Core, updater, datakataloger
   och startregler. Vanliga kataloger är `/opt/ftw`, `~/ftw` och
   `~/forty-two-watts`, men använd den väg som finns på maskinen.
2. Ta en full backup och spara den utanför maskinen. Behåll också Compose-filer,
   overrides och `.env`, så att du kan återställa rätt gamla version.
3. Kontrollera om projektet även kör Mosquitto eller andra tjänster som
   utrustningen behöver. Ordna fortsatt MQTT innan du stoppar hela projektet.
4. Stoppa den gamla installationen med dess riktiga Compose-filer och
   projektnamn. Ett vanligt projekt kan stoppas med `docker compose down`
   från rätt katalog. Använd inte `-v` och radera inga datakataloger.
   Kontrollera även systemd, timers och egna skript som kan skapa eller starta
   den igen. Stäng inte av Docker som helhet.
5. Följ [Docker-guiden för nya FTW](https://github.com/srcfl/ftw/blob/master/docs/native-beta.md#docker).
   Använd en ny katalog, normalt `~/ftw-local`, och en tom datakatalog.
   Återanvänd inte den gamla datakatalogen eller en redan befintlig testkatalog
   utan att först kontrollera vad den innehåller.
6. Ställ in anläggningen och kontrollera version, enheter, mätvärden och plan.
   Kontrollera att bara en Core körs och styr utrustningen. Upprepa kontrollen
   efter en planerad omboot.

För att gå tillbaka: stoppa nya FTW först. Starta sedan det bevarade gamla
projektet med dess sparade filer och versionsval. Återaktivera bara de
startregler du själv stängde av. Den gamla och nya installationen behåller var
sin data.

**Port 8080 är ingen spärr mot dubbel styrning.** Äldre Core kan fortsätta
styra även om den inte kan starta sin webbsida. En enda synlig webbsida är
därför inget bevis på att bara en FTW körs.

## Uppdatera när du redan kör nya FTW

### Native med launcher

Kör på FTW-maskinen:

```bash
ftw status
ftw update --channel beta
ftw status
journalctl -u ftw --since "10 minutes ago" --no-pager
```

`--channel beta` väljer beta och försöker uppdatera. När rätt kanal redan är
sparad räcker `ftw update` nästa gång. Den nya native-webbsidan visar version
och uppdateringsbesked; själva uppdateringen sker via kommandot.

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

## För agenter som hjälper till

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
