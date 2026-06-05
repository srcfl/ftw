# Writing a Lua driver with Claude Code

This guide is a hands-on recipe for contributors who want to use
[Claude Code](https://docs.claude.com/claude-code) to bootstrap a new
42W Lua driver from a vendor-supplied Modbus (or MQTT) register map.
It assumes you are porting one device into `drivers/<name>.lua` and
shipping it as a PR.

Claude Code is very good at the mechanical translation work
(register map -> Lua). It is less good at catching 42W-specific
conventions (sign flip, capability gates, watchdog safety). The prompts
and checklist below exist to close that gap.

For the underlying API and lifecycle, read `docs/writing-a-driver.md`
first. This guide layers on top of it.

## 1. Prerequisites

Before you start:

- `forty-two-watts` checked out at a recent `master`.
- Go 1.26+ installed. Run `cd go && go test ./internal/drivers/...`
  once to confirm the toolchain works end-to-end.
- `make dev` starts cleanly and the sims at
  `http://localhost:8080` show non-zero telemetry. If this does not
  work, fix it before layering a new driver on top.
- [Claude Code](https://docs.claude.com/claude-code) installed and
  authenticated (`claude --version` returns a version).
- A vendor-provided register map as a PDF, markdown, or HTML page.
  The better the map, the better the first draft.

## 2. Prompt recipe

Open a Claude Code session in the repo root:

```bash
cd /Users/fredde/repositories/forty-two-watts
claude
```

### 2.1 Attach the register map

If the map is a PDF, drop it into the session:

```
Read the attached register map for the <vendor> <model>.
```

If it is a web page or markdown, paste the content verbatim between
triple backticks. Include the scaling and endianness notes — those
are the fields Claude Code most often gets wrong.

### 2.2 The starter prompt

Use this verbatim, filling in the name and vendor:

```
Read drivers/sungrow.lua and docs/writing-a-driver.md. Using that as
the template, generate a new Lua driver at drivers/<name>.lua for the
<vendor> <model> Modbus register map above. Follow 42W's v2.1 host
API — host.log(level, msg), host.decode_u32_be/le, host.set_make,
host.set_sn, host.emit_metric. Add the DRIVER table, all five
lifecycle functions (driver_init, driver_poll, driver_command,
driver_default_mode, driver_cleanup), pcall every modbus_read.
```

For an MQTT driver, swap the reference to `drivers/ferroamp.lua` and
mention `host.mqtt_sub` / `host.mqtt_pub` / `host.mqtt_messages` in
place of the Modbus helpers.

### 2.3 Loop it through the catalog loader

As soon as the file exists, tell Claude Code to verify it parses:

```
Now run `cd go && go test -count=1 -run TestLuaDriverLifecycle
./internal/drivers/` and fix any load/runtime errors.
```

This test is fast (<2 s). It catches the most common mistakes: the
DRIVER regex failing, missing lifecycle functions, and syntax errors
in the Lua source.

### 2.4 Ask for the config stanza

Once the file compiles:

```
Generate the config.yaml entry I need to add for this driver — name,
lua path, capability block (modbus or mqtt), and the standard
battery_capacity_wh / is_site_meter fields.
```

Paste the result into `config.yaml` under `drivers:` and restart.

## 3. Porting checklist

Work through this list before you open the PR. Claude Code will draft
most of these; your job is to verify each one. This is the same
checklist used for porting hugin drivers — ticking every box is the
bar for a mergeable driver.

```
[ ] Copy the reference driver (sungrow.lua for Modbus,
    ferroamp.lua for MQTT) as the starting point; cite the source path
    in the file header comment.
[ ] Add the DRIVER table at the top of the file (see sungrow.lua
    lines 1-20 for template shape).
[ ] Replace every `host.log(msg)` with `host.log("info", msg)` /
    "debug" / "warn" / "error".
[ ] Rename `host.decode_u32(...)` to `host.decode_u32_be(...)` or
    `_le` as the register map specifies.
[ ] Rename `host.decode_i32(...)` to `host.decode_i32_be(...)` or
    `_le` as the register map specifies.
[ ] Add `host.set_make("<Manufacturer>")` in `driver_init`.
[ ] Add `host.set_sn(...)` as soon as the SN register/topic is read.
[ ] Convert diagnostic scalars (temperatures, DC voltages, heatsink,
    frequency, MPPT currents) to `host.emit_metric(...)` calls.
[ ] Apply site convention: positive = into site. Invert any sign
    the vendor reports the "wrong" way.
[ ] Wrap every `host.modbus_read` in `pcall`; never crash the driver
    on a bad read.
[ ] Implement `driver_default_mode()` that returns the device to
    autonomous self-consumption.
[ ] Implement `driver_cleanup()` that clears cached state and reverts
    any forced modes.
[ ] If read-only, still define `driver_command` returning `false` on
    unknown action.
[ ] `go test -count=1 ./internal/drivers/` passes.
[ ] `go test ./...` passes.
```

## 4. Pitfalls Claude Code falls into

These come up repeatedly; skim the generated file for each one.

- **Wrong `host.log` signature.** Claude Code often defaults to
  hugin-style `host.log(msg)`. 42W requires two args:
  `host.log("info", msg)`. A single-arg call raises a Lua error on
  the first log line.
- **Decoder without endianness suffix.** `host.decode_u32` is not a
  valid call on 42W. It must be `host.decode_u32_be(hi, lo)` or
  `host.decode_u32_le(lo, hi)`. Argument order differs between the
  two — do not copy-paste one and change only the suffix.
- **Missing `host.set_sn`.** Without a serial number the driver falls
  back to endpoint-based identity (`ep:<endpoint>`). Battery models
  and trained state get orphaned the moment the endpoint changes.
  Always read the serial register and call `host.set_sn` once.
- **Missing `driver_default_mode`.** If the EMS crashes, watchdog
  flips the driver offline and calls this function. If it is missing,
  the device is stuck in whatever forced mode the EMS last commanded
  — battery can end up charging flat against a dead grid target.
- **Bare `host.modbus_read` calls.** A failed read will raise and
  kill the entire poll cycle. Wrap every call in `pcall` and check
  `ok` before indexing the returned table. See
  `drivers/sungrow.lua:127-129` for the canonical pattern.
- **Wrong sign convention.** Most inverters report PV as a positive
  number and battery charge/discharge with the vendor's own
  convention. 42W needs site convention (`docs/site-convention.md`):
  PV always negative, battery positive when charging, meter positive
  when importing from grid. Do the flip in the driver, once, at
  the boundary.
- **`soc` emitted as a percent.** The EMS expects a 0.0-1.0
  fraction. Divide by 100 if the register is 0-100.

## 5. Iteration loop: sim + curl + log tail

Once the file parses and the catalog picks it up, validate it against
a running stack. Open three terminals.

Terminal 1 — start the sims and the main app:

```bash
make dev
```

Terminal 2 — watch the driver-specific log lines (slog key is `driver=<name>`):

```bash
tail -f state/forty-two-watts.log | grep driver=<name>
```

Terminal 3 — hit the status endpoint and the catalog:

```bash
curl -s localhost:8080/api/status | jq .
curl -s localhost:8080/api/drivers/catalog | jq '.entries[] | select(.id=="<your-id>")'
curl -s localhost:8080/api/series/catalog | jq .
```

What you are looking for:

- Your driver appears in `/api/drivers/catalog` with the metadata
  you put in the DRIVER table.
- `/api/status` shows non-zero telemetry for whichever channel
  (meter, pv, battery) your driver emits.
- PV power is **negative** when the sun is up (site convention).
- No `Lua runtime error` entries in the log.
- `tick_count` for your driver advances on every poll — if it
  stalls, the watchdog will flip the driver offline after 60 s.

Save any vendor Modbus server traces or MQTT payload captures in a
gist and paste them back into Claude Code: "Here is the real
response from <device>. Does the driver parse it correctly?" That
pairs well with the iteration loop.

For the full live-test recipe — including Pi deploy, rsync +
systemctl workflow, and failure-mode rehearsals — see
`docs/testing-drivers-live.md` (sibling doc).

## 6. Promoting the driver

Once the checklist is green and the sim loop confirms telemetry
flows:

```bash
git checkout -b <issue>-<name>-driver
git add drivers/<name>.lua
git commit -m "drivers: add <vendor> <model> Lua driver"
git push -u origin HEAD
gh pr create --fill
```

PR reviewers will check:

- The DRIVER metadata block is well-formed (catalog regex parses it
  — confirmed by `TestCatalog`).
- All five lifecycle functions are present.
- Every `host.modbus_read` is inside a `pcall`.
- `host.set_make` and `host.set_sn` are called in `driver_init`.
- Site sign convention is applied at the boundary, with a comment
  citing `docs/site-convention.md` near any sign flip.
- `driver_default_mode` returns the device to autonomous
  self-consumption (not idle, not off).
- `go test ./...` passes in CI.
- File header comment cites the source register map (PDF name or
  URL) and any hugin/evcc driver it was adapted from.
- Tested-on-hardware notes are in the PR description: which model,
  firmware version, observed steady-state numbers.

A "read-only first, control later" PR is welcome — ship telemetry
alone, then follow up with `driver_command` when you have a device
to write to.
