# Ferroamp per-driver SoC bounds via YAML Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `CHARGE_CEIL_SOC` and `DISCHARGE_FLOOR_SOC` in `drivers/ferroamp.lua` configurable via the operator's `config.yaml` driver block, with backwards-compatible defaults.

**Architecture:** Lua-only change. The generic `Config map[string]any` already flows from YAML through `Driver.Init(ctx, config)` into the Lua table received by `driver_init(config)`. Two new optional fields are read and override the file-scope locals. Defaults preserved when fields are absent.

**Tech Stack:** Lua 5.1 (gopher-lua), YAML config, no Go-side changes.

**Spec:** `docs/superpowers/specs/2026-05-27-ferroamp-soc-bounds-config-design.md`

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `drivers/ferroamp.lua` | Modify (around line 276, `driver_init`) | Parse two new optional config fields, override file-scope locals, validate ranges, log applied values and warnings |
| `docs/configuration.md` | Modify | Document the two new fields with example block |
| `CLAUDE.md` | Modify (Ferroamp section if present, else "Adding a new driver" notes) | One-line note that bounds are config-tunable |

---

## Task 1: Lua config parsing in `driver_init`

**Files:**
- Modify: `drivers/ferroamp.lua` around line 276 (`driver_init`)

- [ ] **Step 1: Read current `driver_init` to confirm context**

Run: `sed -n '270,330p' drivers/ferroamp.lua`

Expected: see `function driver_init(config)` starting at line 276, with existing `config.skip_battery` and `config.eso_capacity_kwh` blocks.

- [ ] **Step 2: Add config parsing block immediately AFTER the existing `eso_capacity_kwh` block, BEFORE the MQTT subscribe section**

Identify insertion point: right after the `end` that closes the `eso_capacity_kwh` if-block (currently around line 306).

Insert this block:

```lua
    -- Per-driver SoC bounds — operator override for the file-scope
    -- DISCHARGE_FLOOR_SOC and CHARGE_CEIL_SOC defaults. Lets sites
    -- with different chemistry / longevity preferences tune the
    -- window without forking the driver. Ferroamp's own BMS still
    -- protects against overcharge / deep discharge regardless of
    -- what we set here.
    if config and config.charge_ceil_soc ~= nil then
        local v = tonumber(config.charge_ceil_soc)
        if v and v > 0 and v <= 1.0 then
            CHARGE_CEIL_SOC = v
            host.log("info", string.format(
                "Ferroamp: CHARGE_CEIL_SOC = %.3f (from config)", v))
        else
            host.log("warn", string.format(
                "Ferroamp: charge_ceil_soc=%s ignored (must be 0 < v <= 1)",
                tostring(config.charge_ceil_soc)))
        end
    end

    if config and config.discharge_floor_soc ~= nil then
        local v = tonumber(config.discharge_floor_soc)
        if v and v >= 0 and v < 1.0 then
            DISCHARGE_FLOOR_SOC = v
            host.log("info", string.format(
                "Ferroamp: DISCHARGE_FLOOR_SOC = %.3f (from config)", v))
        else
            host.log("warn", string.format(
                "Ferroamp: discharge_floor_soc=%s ignored (must be 0 <= v < 1)",
                tostring(config.discharge_floor_soc)))
        end
    end

    if CHARGE_CEIL_SOC <= DISCHARGE_FLOOR_SOC then
        host.log("warn", string.format(
            "Ferroamp: CHARGE_CEIL_SOC (%.3f) <= DISCHARGE_FLOOR_SOC (%.3f) — usable charge window is empty",
            CHARGE_CEIL_SOC, DISCHARGE_FLOOR_SOC))
    end
```

- [ ] **Step 3: Verify syntactic correctness by running the existing e2e test (which loads the driver)**

Run: `cd go && go test ./test/e2e/ -run TestE2E -count=1 -v 2>&1 | tail -20`

Expected: PASS. The e2e test starts the Ferroamp driver against a sim — if the Lua syntax is broken, the driver fails to load and the test fails with a Lua error in the log.

If TestE2E doesn't exist by that exact name, find the relevant test with:
`cd go && grep -rn 'ferroamp' ./test/e2e/ | head -5`

and run whichever test exercises ferroamp.

- [ ] **Step 4: Run the full Ferroamp-related Go tests**

Run: `cd go && go test ./internal/drivers/... -count=1`

Expected: PASS.

---

## Task 2: Documentation — `docs/configuration.md`

**Files:**
- Modify: `docs/configuration.md`

- [ ] **Step 1: Locate the Ferroamp section in `docs/configuration.md`**

Run: `grep -n -i 'ferroamp\|eso_capacity' docs/configuration.md | head -10`

If a Ferroamp section exists, append the new fields to its example block. If only a generic "drivers" section exists, add a Ferroamp subsection.

- [ ] **Step 2: Add the two new fields to the example block (or create a new example block if none exists)**

Insertion content:

```yaml
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    config:
      # Optional. Defaults shown — both fields are read by the Lua
      # driver to gate per-ESO charge/discharge dispatch. Ferroamp's
      # own BMS still protects against overcharge / deep discharge,
      # so these are tuning knobs for cell-balancing / longevity
      # preferences, not safety limits.
      charge_ceil_soc: 0.95      # exclude ESOs at or above this SoC from charge dispatch
      discharge_floor_soc: 0.15  # exclude ESOs at or below this SoC from discharge dispatch
```

- [ ] **Step 3: Verify the YAML in the docs is well-formed**

Run: `grep -A 20 'charge_ceil_soc' docs/configuration.md | head -25`

Expected: shows the new block with correct indentation.

---

## Task 3: One-line note in CLAUDE.md

**Files:**
- Modify: `CLAUDE.md` (top-level project orientation doc)

- [ ] **Step 1: Locate the Ferroamp or driver-related section**

Run: `grep -n -i 'ferroamp\|drivers/' CLAUDE.md | head -10`

- [ ] **Step 2: Add a one-line note in the most relevant location**

If there is a Ferroamp-specific bullet, append: `Battery SoC bounds (CHARGE_CEIL_SOC / DISCHARGE_FLOOR_SOC) are config-tunable per driver instance — see docs/configuration.md.`

If only a generic driver section exists, no edit needed beyond the docs/configuration.md update.

---

## Task 4: Commit and verify

**Files:**
- All changes from Tasks 1-3

- [ ] **Step 1: Verify everything builds and tests pass**

Run: `make verify 2>&1 | tail -5`

Expected: `verify: vet + test + build clean`

- [ ] **Step 2: Stage and inspect the diff**

Run: `git add drivers/ferroamp.lua docs/configuration.md CLAUDE.md && git diff --cached --stat`

Expected: 1-3 files changed, modest line count.

- [ ] **Step 3: Commit**

```bash
git commit -m "$(cat <<'EOF'
feat(ferroamp): expose CHARGE_CEIL_SOC and DISCHARGE_FLOOR_SOC as YAML config

Both thresholds gate the per-ESO charge/discharge-capable count
that drives dispatch scaling. Operators with different longevity
preferences or chemistry can now tune the bounds without forking
the driver. Defaults preserved (0.95 / 0.15) — existing configs
unaffected.

Example:
  - name: ferroamp
    config:
      charge_ceil_soc: 1.0       # charge all the way to 100%
      discharge_floor_soc: 0.05  # discharge down to 5%

Spec: docs/superpowers/specs/2026-05-27-ferroamp-soc-bounds-config-design.md

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

Expected: pre-commit hook runs `make verify`, passes, commit lands.

---

## Task 5: Live deployment + verification (operator-driven, post-merge)

This task is operator-side, not part of the PR itself. Listed for completeness.

- [ ] **Step 1: Wait for PR to merge to master and a release to ship via Changesets**

- [ ] **Step 2: Operator (Fredrik) adds the field to live `config.yaml` via UI or `POST /api/config`:**

```yaml
- name: ferroamp
  is_site_meter: true
  config:
    charge_ceil_soc: 1.0
```

- [ ] **Step 3: Verify in logs that the override took effect**

Look for: `Ferroamp: CHARGE_CEIL_SOC = 1.000 (from config)`

- [ ] **Step 4: Observe Ferroamp SoC climbing past 95% during PV-surplus hours**

Check `/api/status` `.drivers.ferroamp.bat_soc`. Should now reach 0.98+ given enough PV.

---

## Self-review notes

**Spec coverage:**
- ✅ YAML surface (Task 2)
- ✅ Lua implementation (Task 1)
- ✅ Validation rules (Task 1 step 2 — explicit range checks + warnings)
- ✅ No Go-side changes (confirmed in plan architecture)
- ✅ Docs (Task 2 + Task 3)
- ✅ Risk: BMS-protects mitigation documented in Task 2 docs block
- ⚠ Not covered as a separate task: lua unit test. Spec says "skip — mechanically trivial, observable end-to-end." Plan follows that decision.

**Placeholders:** None — all code blocks contain final content.

**Type consistency:** N/A (Lua, no types).
