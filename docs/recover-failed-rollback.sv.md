# Akut återställning efter en misslyckad rollback

Använd detta när en FTW-enhet fortfarande svarar på ping men webbgränssnittet
eller containrarna inte startar efter rollback. Målet är först att bevara allt
som kan innehålla historik och därefter få tjänsten att starta utan att kasta
SQLite-data.

## 1. Ändra inget innan data är kopierad

- Kör inte en ny rollback, ominstallation eller migration.
- Radera inte `state.db-wal`, `state.db-shm`, `snapshots/` eller `cold/`.
- Formatera inte SD-kortet och starta inte om enheten upprepade gånger.
- Om SSH inte svarar, anslut skärm och tangentbord lokalt. Om inte heller det
  fungerar: stäng av, ta ur SD-kortet och låt oss klona det på en annan
  Linux-dator innan fler startförsök.

## 2. Hitta installationen och samla status

Kör följande via SSH eller den lokala terminalen:

```bash
if [ -f "$HOME/ftw/docker-compose.yml" ]; then
  cd "$HOME/ftw"
elif [ -f "$HOME/forty-two-watts/docker-compose.yml" ]; then
  cd "$HOME/forty-two-watts"
else
  echo "Installationskatalogen hittades inte"
  exit 1
fi

pwd
docker compose ps -a
MAIN="$(docker compose config --services | grep -E '^(ftw|forty-two-watts)$' | head -n 1)"
test -n "$MAIN" || { echo "FTW-servicen hittades inte"; exit 1; }
docker compose logs --no-color --tail=200 "$MAIN" ftw-updater 2>&1
sudo ls -la data data/snapshots data/cold 2>&1
sudo du -sh data data/snapshots data/cold 2>&1
```

Fotografera eller kopiera hela utskriften. Att en av de två huvudservicarna
inte finns är normalt.

## 3. Gör en orörd räddningskopia

Anslut ett USB-minne eller en USB-disk med mer ledigt utrymme än `data/`.
Ersätt `/media/usb` nedan med sökvägen där disken är monterad:

```bash
RESCUE=/media/usb
test -d "$RESCUE" || { echo "Räddningsdisken är inte monterad"; exit 1; }
docker compose stop
sudo tar --numeric-owner -C "$PWD" \
  -czf "$RESCUE/ftw-rescue-$(date +%Y%m%d-%H%M%S).tgz" \
  data docker-compose.yml
sync
ls -lh "$RESCUE"/ftw-rescue-*.tgz
```

Fortsätt inte om arkivet saknas eller är oväntat litet. Behåll det oförändrat;
det kan innehålla återvinningsbar historik i SQLite WAL, gamla snapshots eller
Parquet-filer under `cold/`.

## 4. Försök starta FTW utan att radera WAL

Den äldre rollbacken kunde göra databasen root-ägd trots att FTW kör som
uid 100/gid 101. Efter att räddningskopian är verifierad:

```bash
sudo chown 100:101 data
sudo find data -maxdepth 1 \
  \( -name 'state.db' -o -name 'state.db-*' -o -name 'config.yaml' \) \
  -exec chown 100:101 {} +

MAIN="$(docker compose config --services | grep -E '^(ftw|forty-two-watts)$' | head -n 1)"
test -n "$MAIN" || { echo "FTW-servicen hittades inte"; exit 1; }
docker compose up -d "$MAIN"
sleep 10
docker compose ps -a
docker compose logs --no-color --tail=200 "$MAIN"
curl -fsS http://127.0.0.1:8080/api/health
```

Om health-kommandot lyckas: gör ingen ny rollback. Behåll räddningskopian och
uppgradera installationen först när den korrigerade stabila releasen är
publicerad.

Om tjänsten fortfarande inte startar, stoppa den igen och skicka
räddningsarkivet samt loggutskriften för analys:

```bash
docker compose stop "$MAIN"
```

Skapa inte en tom `state.db` och radera inte WAL-filerna för att få bort ett
felmeddelande; det kan göra kvarvarande historik omöjlig att rädda.
