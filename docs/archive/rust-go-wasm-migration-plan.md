# Archived migration plan: Rust to Go + WASM drivers

Archived on 2026-06-05. The Rust to Go migration is complete, and the
production driver runtime is now Go + Lua. This document is historical
context only; it still mentions redb, WASM drivers, and paths that no longer
exist.

Original heading:

# Migration Plan: Rust → Go + WASM-drivers

## Varför vi migrerar

**Konsensus efter utvärdering:**

1. **Byggtider.** Rust cold-build 90s via Docker cross, Go beräknat ~20s. För
   våra små frekventa deploys är detta betydande. Local iteration blir
   snabbare och smidigare.

2. **Drivers = riktiga program, inte scripts.** Vårt Lua-baserade system
   tvingar driver-författare att pusha binär-parsing till host. Det begränsar
   vad drivers kan göra. Med WASM-drivers blir varje driver en fullständig
   Rust-applikation med tillgång till hela crates.io-ekosystemet.

3. **Enklare operations.** Pure Go → statisk binär utan musl-drama, paho
   MQTT och simonvetter Modbus är battle-tested, `net/http` behöver ingen
   extra HTTP-lib.

4. **Lägre tröskel för bidrag.** Go-utvecklare är fler och Go-kod är
   lättare att onboarda på än Rust med lifetimes och borrow checker.

**Vad vi inte får tillbaka:**

- Borrow checker som säkerhetsnät (ersätts med diciplinerad testning + race
  detector)
- Algebraic types (ersätts med typade structs + metoder)

Acceptabel förlust givet vinsterna.

---

## Princip: drivers gör allt de kan

> **Drivers ska vara så *fat* som möjligt. Host exposar bara kapabiliteter
> (I/O, tid, logging), inte funktionalitet. Allt som är driver-specifikt —
> protokoll-parsing, state machines, retry-logik, kommando-translation —
> hör hemma i drivern.**

Detta står i skarp kontrast till nuvarande Lua-arkitektur där host.decode_*,
host.json_decode etc. måste implementeras för varje format. Med WASM kan
drivers importera riktiga crates (nom, serde_json, crc, modbus-protokoll-
parsers etc.).

---

## Library-val

Princip: **pure Go, minimala deps, mature libraries**.

| Behov | Rust idag | Go-val | Motivering |
|---|---|---|---|
| HTTP-server | tiny_http (extern) | stdlib `net/http` | Inbyggt, mer välskött |
| Config (YAML) | serde_yaml | `gopkg.in/yaml.v3` | De facto-standard, stabilt |
| JSON | serde_json | stdlib `encoding/json` | Inbyggt, snabbt nog |
| State / persistent KV | redb | **TBD — `bbolt` vs SQLite via `modernc.org/sqlite`** | Se nedan |
| Historik / time-series | redb (custom tiered) | **SQLite** via `modernc.org/sqlite` | SQL-frågor betydligt enklare än vår custom tiered-kod |
| File-watcher | notify | `fsnotify/fsnotify` | Pure Go, samma API-mönster |
| Strukturerad loggning | tracing | stdlib `log/slog` | Inbyggt i Go 1.21+ |
| MQTT-klient | rumqttc | `eclipse/paho.mqtt.golang` | Stabilt, välanvänt |
| MQTT-broker (för sim) | — | `mochi-mqtt/server` | Pure Go, embeddable |
| Modbus TCP (client+server) | egen + egen | `simonvetter/modbus` | Både client och server i en lib; goburrow har bara client |
| WASM-runtime | — | `tetratelabs/wazero` | **Pure Go, zero CGo**, production-ready |
| Signal handling | ctrlc | stdlib `os/signal` | Inbyggt |
| PI-controller | `pid` crate | egen port (~50 LOC) | Lite kod, inget värde i dependency |

### State DB — bbolt eller SQLite?

**bbolt** (`go.etcd.io/bbolt`):
- ✅ Pure Go, zero deps
- ✅ Transaktionell, ACID
- ✅ Samma mental model som redb (key-value, buckets)
- ❌ Ingen query-förmåga, custom aggregering som idag

**SQLite** (`modernc.org/sqlite`):
- ✅ Pure Go (cgo-fri port), battle-tested implementation
- ✅ SQL för range queries, bucketed aggregates, downsampling
- ✅ **Ersätter vår custom tiered-historik med SQL**:
  ```sql
  -- Warm aggregation: 15-min buckets from hot tier
  INSERT INTO history_warm
    SELECT (ts_ms/900000)*900000 + 450000, AVG(grid_w), AVG(pv_w), ...
    FROM history_hot WHERE ts_ms < ?
    GROUP BY ts_ms/900000;
  DELETE FROM history_hot WHERE ts_ms < ?;
  ```
- ✅ Bättre för historik-vyer i UI:n (range queries med filter)
- ❌ Något större binary (~5 MB till)

**Beslut:** **SQLite**. Historik är huvudanvändningsfallet för persistent
storage, och SQL-frågor är strikt bättre än vår custom tiered-kod. Config och
battery-modeller är små JSON-blobbar som går fint in samma DB.

### MQTT-broker för tester

`mochi-mqtt/server` låter oss embedda en fullständig MQTT-broker i tester
och i Ferroamp-simulatorn. Ingen extern Mosquitto behövs för lokal utveckling.

---

## WASM-driver-ABI

Skrivs med **WASI preview 1** (mest kompatibelt nu). Migrera till component
model (WASI 0.3) när stabilt.

### Exports som en driver måste implementera

```
(module
  ;; Lifecycle
  (export "driver_init"     (func (param i32 i32) (result i32)))  ;; config_ptr, config_len → status
  (export "driver_poll"     (func (result i32)))                  ;; returns next_poll_interval_ms
  (export "driver_command"  (func (param i32 i32) (result i32)))  ;; cmd_json_ptr, cmd_json_len
  (export "driver_default"  (func (result i32)))                  ;; revert to autonomous mode
  (export "driver_cleanup"  (func))

  ;; Memory export required by WASI
  (export "memory" (memory 1))

  ;; WASM alloc/free — host calls these to copy strings into driver memory
  (export "alloc" (func (param i32) (result i32)))
  (export "dealloc" (func (param i32 i32)))
)
```

### Host imports driver kan anropa

```
(module
  ;; Core
  (import "host" "log"              (func (param i32 i32 i32)))  ;; level, ptr, len
  (import "host" "millis"           (func (result i64)))
  (import "host" "set_poll_interval" (func (param i32)))
  (import "host" "emit_telemetry"   (func (param i32 i32)))      ;; json_ptr, json_len
  (import "host" "set_sn"           (func (param i32 i32)))
  (import "host" "set_make"         (func (param i32 i32)))

  ;; MQTT (capability; host injects based on driver config)
  (import "host" "mqtt_subscribe"   (func (param i32 i32) (result i32)))
  (import "host" "mqtt_publish"     (func (param i32 i32 i32 i32) (result i32)))
  (import "host" "mqtt_messages"    (func (param i32 i32) (result i32)))
    ;; writes pending messages to buffer, returns length

  ;; Modbus TCP (capability)
  (import "host" "modbus_read"      (func (param i32 i32 i32 i32) (result i32)))
    ;; addr, count, kind (0=holding 1=input), out_ptr → bytes_written
  (import "host" "modbus_write"     (func (param i32 i32) (result i32)))
    ;; addr, value
  (import "host" "modbus_write_multi" (func (param i32 i32 i32) (result i32)))
    ;; addr, values_ptr, count

  ;; HTTP (for future price/weather drivers)
  (import "host" "http_get"         (func (param i32 i32 i32) (result i32)))

  ;; Standard WASI imports (stdout for println!, clock, random, etc.)
  (import "wasi_snapshot_preview1" ...)
)
```

**Viktigt:** Host **ger inte driver:n decode-hjälp**. Drivers gör sin egen
binär-parsing med vanliga Rust-crates.

### Kapabiliteter

Varje driver får en capability-set baserat på config:

```yaml
drivers:
  - name: ferroamp
    wasm: drivers/ferroamp.wasm
    capabilities:
      mqtt:
        host: 192.168.1.153
        port: 1883
        username: extapi
        password: ferroampExtApi
      # ingen modbus — så modbus_* blockeras

  - name: sungrow
    wasm: drivers/sungrow.wasm
    capabilities:
      modbus:
        host: 192.168.1.10
        port: 502
        unit_id: 1
```

Detta ger säker isolation — en driver kan inte läsa filer, öppna godtyckliga
sockets, eller komma åt andra drivers' MQTT-kopplingar.

---

## Projektstruktur

```
forty-two-watts/
├── go/                              # ← Ny Go-implementation
│   ├── cmd/
│   │   ├── forty-two-watts/         # Huvudbinär
│   │   ├── sim-ferroamp/            # MQTT-broker + Ferroamp-sim
│   │   └── sim-sungrow/             # Modbus TCP-sim
│   ├── internal/
│   │   ├── config/                  # YAML-parsning + validering
│   │   ├── state/                   # SQLite + queries
│   │   ├── telemetry/               # DER store + Kalman
│   │   ├── energy/                  # Wh-integration
│   │   ├── control/                 # PI + dispatch + fuse guard
│   │   ├── battery/                 # ARX(1) RLS + cascade + self-tune
│   │   ├── drivers/                 # wazero-runtime + lifecycle
│   │   ├── api/                     # net/http endpoints
│   │   ├── ha/                      # HA MQTT bridge
│   │   ├── mqtt/                    # MQTT-klient wrapper
│   │   └── modbus/                  # Modbus TCP wrapper
│   ├── go.mod
│   └── go.sum
│
├── wasm-drivers/                    # ← WASM-drivers som Rust-projekt
│   ├── ferroamp/                    # Rust → wasm32-wasip1
│   │   ├── Cargo.toml
│   │   └── src/lib.rs
│   └── sungrow/
│       ├── Cargo.toml
│       └── src/lib.rs
│
├── web/                             # UI (oförändrat, serveras från Go)
│   ├── index.html
│   ├── app.js
│   ├── models.js
│   ├── settings.js
│   └── style.css
│
├── src/                             # ← Rust-kod (behålls på master)
├── drivers/                         # ← Gamla Lua-drivers (obsoletas gradvis)
└── docs/                            # Uppdateras under migration
```

---

## Testning — lokalt, utan hårdvara

**Två simulatorer** gör att hela systemet kan köras på denna Mac utan att
röra RPi:n eller faktisk hårdvara.

### Ferroamp-simulator

`go/cmd/sim-ferroamp/` startar:
1. Embedded MQTT-broker på :1883
2. Ferroamp-fake som publicerar realistiska `extapi/data/ehub` etc.
3. Lyssnar på `extapi/control/request` och svarar med `extapi/result`
4. Har internal state (SoC, PV generation, grid meter) med first-order dynamik

### Sungrow-simulator

`go/cmd/sim-sungrow/` startar:
1. Modbus TCP-server på :5502
2. Realistiska holding+input registers för SH-serien
3. Skrivbara registers (13050 force cmd, 13051 power) uppdaterar internt state
4. Respons-latens + overshoot simulerad

### Dev-loop

```bash
# Terminal 1 — broker + Ferroamp
go run ./go/cmd/sim-ferroamp

# Terminal 2 — Sungrow
go run ./go/cmd/sim-sungrow

# Terminal 3 — huvudapp, pekar på localhost
go run ./go/cmd/forty-two-watts ./config.local.yaml

# Terminal 4 — UI
open http://localhost:8080
```

Allt test-data är dynamiskt och reagerar på dispatch-kommandon i
simulatorerna, så cascade-controllern, battery models osv. kan valideras
ordentligt utan att RPi:n eller riktiga batterier är inblandade.

---

## Migreringsfaser

### Fas 0: Scaffold (detta PR)
- `go-port` branch skapad
- MIGRATION_PLAN.md (detta dokument)
- Go-modul + directory-struktur
- Lib-val dokumenterat

### Fas 1: Simulatorer
- `cmd/sim-ferroamp` — embedded broker + Ferroamp-fake
- `cmd/sim-sungrow` — Modbus-server med register-karta
- Båda verifierade manuellt via mosquitto_sub / modpoll

### Fas 2: Core backend
- `internal/config` — YAML + validering + test
- `internal/state` — SQLite schema + queries + migration
- `internal/telemetry` — DerStore + Kalman + tests
- `internal/energy` — Wh-integration + day rollover + tests

### Fas 3: HTTP API
- `internal/api` — alla endpoints, matchar Rust-versionens kontrakt
- Hot-reload via fsnotify
- Serverar `web/` oförändrat

### Fas 4: WASM-driver-runtime
- `internal/drivers/wasm.go` — wazero integration
- Host-API (log, mqtt_*, modbus_*, emit_telemetry, etc.)
- ABI-test mot minimalt hello-world-driver

### Fas 5: Första WASM-drivern
- `wasm-drivers/ferroamp/` — Rust-lib, kompileras med
  `cargo build --target wasm32-wasip1 --release`
- Full driver-logik inkl JSON-parsing, MQTT-loop
- E2E-test mot Ferroamp-simulator

### Fas 6: Andra WASM-drivern
- `wasm-drivers/sungrow/` — likadant för Sungrow
- E2E mot Sungrow-simulator

### Fas 7: Control loop
- `internal/control` — site PI, dispatch modes, slew, fuse guard
- `internal/battery` — ARX(1), RLS, cascade (confidence-gated)
- `internal/self_tune` — step-response state machine

### Fas 8: HA bridge
- `internal/ha` — autodiscovery + publish + subscribe
- Samma topics som Rust-versionen (drop-in kompatibelt)

### Fas 9: Full e2e-validering
- Scripted test: starta båda simulatorer, kör control loop i 10 min, jämför
  beteende med Rust-versionens loggar
- Inducera grid-step + verifiera response
- Kör self-tune + verifiera ARX(1)-fitning

### Fas 10: Deploy-story
- Makefile: `run-local`, `build-arm64`, `package`
- Kör parallellt på RPi:n (annan port) för stabilitetstest
- Cut-over när nöjda

---

## Risker och mitigationer

| Risk | Mitigation |
|---|---|
| WASM-drivers är svårare att iterera på än Lua | Börja med Rust, lägg senare till TypeScript/AssemblyScript om folk vill |
| wazero-prestanda sämre än mlua | Våra drivers är poll-baserade 1-5 Hz — icke-issue. Benchmark tidigt för säkerhets skull |
| SQLite-ändringar bryter bakåtkompatibilitet | Schema-migrations via simpla numbererade steg, dev på ny DB |
| Community Lua-drivers från Sourceful blir inkompatibla | Designa så Lua-driver-stöd kan läggas till senare som en ytterligare driver-typ |
| Migration tar längre tid än ~2 veckor | Godkänt — Rust-versionen körs oförändrat under tiden |

---

## Status

Se `TODO.md` eller GitHub issues. Fas 0 pågår nu.
