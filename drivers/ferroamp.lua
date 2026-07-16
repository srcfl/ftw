-- ferroamp.lua
-- Ferroamp EnergyHub MQTT driver
-- Emits: PV, Battery, Meter telemetry

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "ferroamp",
  name         = "Ferroamp EnergyHub",
  manufacturer = "Ferroamp",
  version      = "1.0.0",
  protocols    = { "mqtt" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Ferroamp EnergyHub with ESO battery + SSO solar strings (3-phase).",
  homepage     = "https://ferroamp.com",
  authors      = { "FTW contributors" },
  tested_models = { "EnergyHub XL" },
  verification_status = "production",
  verified_by = { "frahlg@homelab-rpi:14d" },
  verified_at = "2026-04-18",
  verification_notes = "In continuous use on 3-phase 16A SE site, MPC + dispatch control loop exercised daily.",
  connection_defaults = {
    port     = 1883,
    username = "extapi",
    password = "ferroampExtApi",
  },
}
--
-- Subscribes to:
--   extapi/data/ehub  - main hub data (grid, frequency, energy counters, PV summary)
--   extapi/data/eso   - battery storage object (SoC, battery power, voltage, current)
--   extapi/data/sso   - solar string object (per-string PV power)
--
-- Ferroamp payload format: {"key": {"val": value}} or {"key": {"L1": v1, "L2": v2, "L3": v3}}
-- Energy counters are in mJ (millijoules); convert to Wh: mJ / 3,600,000
--
-- Sign convention:
--   PV w:      always negative (generation)
--   Battery w: positive = charging, negative = discharging
--   Meter w:   positive = import, negative = export

PROTOCOL = "mqtt"

-- Cached state from each topic
local ehub_data = nil
local sso_data = nil

-- ESO state is per-unit. Ferroamp publishes one extapi/data/eso message
-- per ESO; a single-slot cache always reflected only the most-recently-
-- published unit, halving (or worse) reported battery power in N-ESO
-- installations. Real incident 2026-05-24 at a 2×ESO site: 42's bat_w
-- was exactly half of ehub.pbat, the controller's grid-chase loop fed
-- on the wrong number, and dispatch never converged on grid_target.
-- The fix is to key each ESO by its `id` field and aggregate at emit
-- time; messages without an `id` fall under a synthetic key so
-- single-ESO firmware / sims keep behaving identically.
local eso_data_by_id = {}  -- id -> payload table
local eso_ts_by_id   = {}  -- id -> last arrival ms

-- Last-arrival timestamp per topic (host.millis()). The EnergyHub
-- normally publishes ehub at ~1 Hz; if it goes silent (power off,
-- fuse blow, broker partition) the cached tables above stay
-- populated. Without per-topic age checks the driver would re-emit
-- last-known values on every poll, host.emit would re-stamp
-- LastSuccess, and the watchdog could not flip the driver offline.
-- Real incident: 2026-05-02 fuse blow left ferroamp emitting
-- pv_w=-3996.7040 / meter_w=-7294.0490 identical to four decimals
-- for 30+ minutes while the EnergyHub itself was unpowered.
local ehub_ts = 0
local sso_ts  = 0

-- ehub.soc cache. Ferroamp's ehub topic emits the system SoC field
-- on a coarser cadence than the other ehub fields (~1 in 9 messages
-- on the firmware we tested 2026-05-26, ~once every ~9s). If we read
-- it directly off the latest ehub_data cache we'd see it only when
-- the most recent message happened to include it, otherwise nil —
-- and battery.soc would flap between the ehub value and the fallback
-- every poll. Cache the last-observed soc and its timestamp so it
-- stays the authoritative reading until it ages out.
local last_ehub_soc    = nil
local last_ehub_soc_ts = 0
local EHUB_SOC_STALE_MS = 60000  -- ~6× the observed publish interval

-- Treat cached topic data as stale beyond this age. EnergyHub
-- publishes ehub at ~1 Hz and eso/sso slightly slower; 30 s gives
-- generous slack for a WiFi blip or broker reconnect without
-- flipping the driver offline.
local STALE_AFTER_MS = 30000

-- Optional config knob: when `skip_battery` is true the driver will
-- NOT emit battery telemetry even when the ESO/pbat fields are
-- present on the wire. Useful for dev setups that want a PV-only
-- dashboard fed by the otherwise full-featured Ferroamp sim.
local SKIP_BATTERY = false
local last_control_mode = nil

-- Optional config knob: per-ESO usable capacity in kWh, keyed on the
-- ESO id string (matches the `id.val` field in extapi/data/eso). When
-- provided, battery.soc is reported as a capacity-weighted aggregate
-- instead of the flat arithmetic mean. Useful on heterogeneous clusters
-- where Ferroamp's own app shows a different number than our mean:
-- e.g. 2×ESO-15 (newer) at 87% + 2×ESO-7.7 (older) at 38.6% averages to
-- 62.8%, but Ferroamp's capacity-weighted view sits closer to 70%+.
-- Format in YAML:
--   eso_capacity_kwh:
--     "26040075": 15.0
--     "21030026": 7.7
-- Units with no entry contribute their soc with weight = average of the
-- configured weights (so a partial config doesn't silently zero them).
local ESO_CAPACITY_KWH = {}

-- Multi-ESO dispatch scaling. The EnergyHub divides a discharge/charge
-- setpoint evenly across ALL ESOs it knows about, including units pinned
-- at their SoC floor/ceiling that physically refuse to respond. On a
-- 4×ESO site with 2 units floored at min SoC, asking for 1.3 kW resulted
-- in only ~0.66 kW delivered — the EHub commanded ~330 W per ESO, the
-- two active units honored it, the two floored ones produced 0. Verified
-- live on 2026-05-26 (Stefan's site). To compensate we count how many
-- ESOs are *currently capable* of the requested direction and pre-scale
-- the outgoing command by N_total / N_capable.
--
-- Margins are 5% inside the typical Ferroamp floor (10%) / ceiling (100%)
-- to avoid oscillating at the edge — within ~2% of its limit an ESO
-- already refuses dispatch, so a 5% margin is comfortably outside the
-- "willing but limited" band.
--
-- Caveats this scaling has and does NOT solve:
--   * Behaviour is empirical (single live observation, firmware vintage
--     unknown). If EHub firmware ever rebalances to active units the
--     scaling double-compensates — diff `eso_dispatch_commanded_w` vs
--     the next tick's `battery.w` to spot that.
--   * The upstream dispatch clamp (config.Driver.max_charge_w / fuse
--     guard, dispatch.go ~1685) caps the *target* power, then we
--     multiply on the wire by up to MAX_DISPATCH_SCALE. That bound
--     protects fuse + inverter rating; if you raise it, also revisit
--     the per-driver caps in config.yaml.
local DISCHARGE_FLOOR_SOC = 0.15
local CHARGE_CEIL_SOC     = 0.95
-- PV-curtail release power (watts). When ComputePVCurtail releases this
-- driver (curtail_disable action), the dispatcher historically wanted us
-- to send `pplim arg=0`. Ferroamp's extapi treats that as "limit PV to
-- 0 W" — same wire bytes as a release would have, opposite semantics —
-- and the inverter sticks at 0 W PV until a portal reset. So we publish
-- pplim arg=PPLIM_RELEASE_W instead, which the operator sets to the
-- inverter's nominal max (e.g. 15000 for a 15 kW SSO). Default 0 means
-- "do not publish a release at all", which preserves whatever pplim
-- Ferroamp last received. Override via config.pplim_release_w in
-- config.yaml. 2026-05-27 incident: dispatching `pplim arg=0` bricked
-- the live SE4 site for 30+ minutes; recovery required a Ferroamp
-- portal reset.
local PPLIM_RELEASE_W     = 0

-- Stuck-pplim self-healing watchdog. If the SSO reports DC bus voltage
-- (paneler aktiva) AND zero PV current AND no fault for STUCK_PV_AFTER_MS
-- continuous, we treat that as the sticky-pplim trap signature and
-- auto-publish `pplim arg=PPLIM_RELEASE_W` to recover. Operator opts in
-- by setting PPLIM_RELEASE_W > 0 — without that we just log a warning
-- because we have no safe release value. The recovery has a cooldown
-- (STUCK_PV_RECOVERY_COOLDOWN_MS) so a persistent issue can't loop us
-- into command-spam. 2026-05-27 incident background: PR #367 + #372.
local STUCK_PV_AFTER_MS              = 10 * 60 * 1000  -- 10 minutes
local STUCK_PV_RECOVERY_COOLDOWN_MS  = 5 * 60 * 1000   -- 5 minutes
local STUCK_PV_DC_V_THRESHOLD        = 200             -- volts on the DC bus
local stuck_pv_since_ms              = -1              -- -1 = not stuck
local stuck_pv_last_recovery_ms      = -1              -- -1 = never recovered
local stuck_pv_recovery_count        = 0
-- Cap on the N_total/N_capable multiplier so a transient "only 1 of 4
-- capable" snapshot can't quadruple the on-wire setpoint past inverter
-- rating before the next poll corrects it. 2.0 covers the common
-- 2-of-4 / 1-of-2 cases; deeper imbalance is left under-delivered (a
-- safe failure mode — planner sees the gap and re-plans).
local MAX_DISPATCH_SCALE  = 2.0
local last_eso_count             = 0
local last_eso_discharge_capable = 0
local last_eso_charge_capable    = 0
-- Delivery-ratio scaling. The EnergyHub splits a charge/discharge setpoint
-- evenly across ALL ESOs; a unit that's saturated (CV taper near full, the
-- EHub balancing toward lower-SoC units, voltage/thermal limit) absorbs
-- almost none of its even share, so the pack delivers far less than we ask.
-- Instead of toggling units active/inactive on a power threshold (which
-- flaps when a saturated unit trickles a little — e.g. 170 W against a
-- ~650 W share still reads "charging"), we scale the on-wire setpoint by
-- the inverse of the pack's *delivery efficiency*:
--     eff   = |delivered battery W| / |last on-wire setpoint W|
--     scale = N_eso / N_active   (clamped to MAX_DISPATCH_SCALE)
-- where N_active counts the units that ACTUALLY DELIVERED power in the
-- commanded direction last tick — measured from per-ESO ubat*ibat, NOT from
-- SoC. An ESO at its limit (full on charge, empty on discharge) delivers ~0
-- and is excluded; the up-scale then lifts the delivering units to cover the
-- stuck unit's even share. Because the active units split exactly the
-- commanded power between them, the pack delivers the commanded total and
-- NEVER more — scale is 1.0 when all units deliver, so this can't over-
-- discharge/over-charge (unlike a delivery-efficiency ratio, whose estimate
-- got dragged low by full units during charge and, applied to discharge,
-- drove a 1.66x over-discharge dumping ~2.7 kW to grid — Stefan 2026-06-10).
-- An ESO is "active" if it delivers at least half its 1/N share of the
-- on-wire setpoint (ACTIVE_SHARE_FRAC). The SoC-capable counts below remain,
-- but ONLY for the "every unit floored/ceilinged → idle" guard.
local ACTIVE_SHARE_FRAC = 0.5   -- min fraction of the 1/N share to count a unit "active"
local SCALE_FB_MIN_W = 200      -- only measure delivery when on-wire command >= this
local last_eso_active_charge    = nil  -- units delivering charge last tick (nil until measured)
local last_eso_active_discharge = nil  -- units delivering discharge last tick
local last_on_wire_w = 0        -- magnitude of the last (scaled) on-wire setpoint sent
local last_commanded_w = 0      -- magnitude of the last DISPATCH command (pre-scale); the
                                -- active threshold keys off THIS, not the scaled on-wire
                                -- value, so a rising scale can't inflate the threshold and
                                -- spuriously exclude units that are delivering (feedback bug)
-- Cumulative count of Ferroamp extapi `nak` responses since driver
-- start. Surfaced as the `extapi_nak_count` metric so an operator can
-- alert on any non-zero rate. NAKs are early signals of EMS-side
-- trouble (e.g. "no available ESOs detected in system" preceded the
-- 2026-05-27 brick by minutes).
local extapi_nak_count           = 0
local extapi_ack_count           = 0
-- -1 = "no snapshot yet" sentinel. host.millis() can legitimately
-- return 0 on the very first poll (sub-millisecond since startup), so
-- we can't use 0 to mean "never set".
local last_eso_counts_ms         = -1

----------------------------------------------------------------------------
-- Helpers
----------------------------------------------------------------------------

-- Extract a value from Ferroamp's {"key": {"val": v}} structure.
-- Returns the raw val (string/number), or the field table if no "val" key.
local function extract_val(obj, key)
    if not obj then return nil end
    local field = obj[key]
    if not field then return nil end
    if type(field) == "table" and field.val ~= nil then
        return field.val
    end
    return field
end

-- Sum L1+L2+L3 from a phase table {"L1":..,"L2":..,"L3":..}, or return scalar.
-- Also handles numeric arrays for backwards compatibility.
local function sum_phases(val)
    if val == nil then return 0 end
    if type(val) == "number" then return val end
    if type(val) == "string" then return tonumber(val) or 0 end
    if type(val) == "table" then
        -- Try named keys first (current Ferroamp format)
        if val.L1 or val.L2 or val.L3 then
            return (tonumber(val.L1) or 0) + (tonumber(val.L2) or 0) + (tonumber(val.L3) or 0)
        end
        -- Fall back to numeric array
        local s = 0
        for _, v in ipairs(val) do
            s = s + (tonumber(v) or 0)
        end
        return s
    end
    return 0
end

-- Get a specific phase value from {"L1":..,"L2":..,"L3":..} or array [1,2,3].
local function phase_val(val, phase)
    if val == nil then return 0 end
    if type(val) ~= "table" then return 0 end
    -- Named key (e.g. "L1")
    if val[phase] then return tonumber(val[phase]) or 0 end
    -- Numeric index fallback (L1=1, L2=2, L3=3)
    local idx = ({L1=1, L2=2, L3=3})[phase]
    if idx and val[idx] then return tonumber(val[idx]) or 0 end
    return 0
end

-- Convert Ferroamp mJ counter to Wh
local function mj_to_wh(mj_val)
    local mj = tonumber(mj_val) or 0
    return mj / 3600000
end

-- Prefer the primary topic value, but fall back to the role-specific topic
-- when the primary is missing or reports zero while the fallback has a real
-- magnitude. Some EnergyHub payloads keep pbat/ppv useful on ESO/SSO even
-- when ehub's summary field is zeroed.
local function choose_power(primary, fallback)
    local p = tonumber(primary)
    local f = tonumber(fallback)
    if p ~= nil and math.abs(p) > 0.5 then return p end
    if f ~= nil and math.abs(f) > 0.5 then return f end
    if p ~= nil then return p end
    return f
end

local function eso_battery_power(data)
    if not data then return nil end
    local ubat = tonumber(extract_val(data, "ubat"))
    local ibat = tonumber(extract_val(data, "ibat"))
    if ubat == nil or ibat == nil then return nil end
    return ubat * ibat
end

-- Returns the id of an ESO payload, or "_default_" when the firmware
-- omits the `id` field. The synthetic key keeps single-ESO sims /
-- old firmware on the same code path as a 1-entry map.
local function eso_id_of(data)
    local id = extract_val(data, "id")
    if id == nil or id == "" then return "_default_" end
    return tostring(id)
end

local function sso_power(data)
    if not data then return nil end
    local ppv = extract_val(data, "ppv")
    if ppv ~= nil then return tonumber(ppv) end

    -- The SSO topic does not publish ppv in all External API versions.
    -- It does publish string voltage/current; use their product as a
    -- measured-string fallback. Some firmware reports PV voltage in kV
    -- below 10, so normalize that shape back to volts.
    local upv = tonumber(extract_val(data, "upv"))
    local ipv = tonumber(extract_val(data, "ipv"))
    if upv == nil or ipv == nil then return nil end
    local scale = math.abs(upv) < 10 and 1000 or 1
    return upv * scale * ipv
end

local function publish_auto(trans_id)
    local err = host.mqtt_publish("extapi/control/request",
        string.format('{"transId":"%s","cmd":{"name":"auto"}}', trans_id))
    if not err then last_control_mode = "auto" end
    return err
end

-- Force the EnergyHub to hold the battery at 0 W instead of handing
-- control back to autonomous self-consumption. `auto` reads the
-- house load and discharges to cover it, which silently overrides
-- any planner slot that wants the battery idle so PV surplus can
-- export. `discharge` with arg=0 keeps the inverter in forced mode
-- but locked at zero — equivalent to Sungrow's forced-idle path.
local function publish_idle(trans_id)
    local err = host.mqtt_publish("extapi/control/request",
        string.format('{"transId":"%s","cmd":{"name":"discharge","arg":0}}', trans_id))
    if not err then last_control_mode = "idle" end
    return err
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Ferroamp")

    -- Honour the `skip_battery` config knob if set — the driver stays
    -- otherwise unchanged, but host.emit("battery", …) is skipped so
    -- the rest of the stack (dashboard, models, planner) sees no
    -- battery capability from this instance.
    if config and config.skip_battery then
        SKIP_BATTERY = true
        host.log("info", "Ferroamp: skip_battery=true — battery emission disabled")
    end

    -- Per-ESO capacities for the weighted-SoC aggregation. We accept
    -- whatever map the YAML hands us; ids are coerced to strings so a
    -- user writing them bare (no quotes) still hits the lookup. Values
    -- below 0.1 kWh are ignored — almost certainly a typo, and a tiny
    -- weight near zero distorts the weighted mean.
    if config and config.eso_capacity_kwh then
        local n = 0
        for k, v in pairs(config.eso_capacity_kwh) do
            local kwh = tonumber(v)
            if kwh and kwh >= 0.1 then
                ESO_CAPACITY_KWH[tostring(k)] = kwh
                n = n + 1
            end
        end
        if n > 0 then
            host.log("info", "Ferroamp: loaded capacity weights for " .. n .. " ESO(s) — bat_soc will be capacity-weighted")
        end
    end

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

    -- pplim release watts: see the file-scope PPLIM_RELEASE_W comment for
    -- why this matters (the 2026-05-27 sticky-pplim incident). Operators
    -- who use supports_pv_curtail=true SHOULD set this to the SSO nominal
    -- max so curtail_disable safely publishes a release. Leaving it 0
    -- means "no release published" — safe default, but the operator must
    -- manually clear pplim from the Ferroamp portal after curtail ends.
    if config and config.pplim_release_w ~= nil then
        local v = tonumber(config.pplim_release_w)
        if v and v > 0 then
            PPLIM_RELEASE_W = math.floor(v)
            host.log("info", string.format(
                "Ferroamp: PPLIM_RELEASE_W = %d (from config)", PPLIM_RELEASE_W))
        else
            host.log("warn", string.format(
                "Ferroamp: pplim_release_w=%s ignored (must be > 0)",
                tostring(config.pplim_release_w)))
        end
    end

    -- Subscribe to telemetry topics
    host.mqtt_subscribe("extapi/data/ehub")
    host.mqtt_subscribe("extapi/data/eso")
    host.mqtt_subscribe("extapi/data/sso")

    -- Subscribe to the EMS-side response channel for every command we
    -- publish. We parse `{"status":"ack|nak", ...}` from each message
    -- and count NAKs in a metric for operator alerting. The 2026-05-27
    -- brick was preceded by minutes of `{"status":"nak","msg":"no
    -- available ESOs detected in system"}` that we couldn't see at the
    -- time because the driver subscribed to the wrong topic. Older
    -- code used "extapi/result"; the actual response topic on the
    -- firmwares we've tested is "extapi/control/response".
    host.mqtt_subscribe("extapi/control/response")

    -- Query API version to verify connectivity and external API access
    local version_cmd = '{"transId":"init","cmd":{"name":"extapiversion"}}'
    host.mqtt_publish("extapi/control/request", version_cmd)
    host.log("info", "Ferroamp: sent extapiversion query")

    -- Ensure we start in auto mode (clean state)
    publish_auto("init")
    host.log("info", "Ferroamp: set auto mode on init")
end

function driver_poll()
    local now = host.millis()
    local messages = host.mqtt_messages()
    if not messages then messages = {} end

    -- Process incoming messages and stamp arrival time per topic
    for _, msg in ipairs(messages) do
        local ok, data = pcall(host.json_decode, msg.payload)
        if ok and data then
            if msg.topic == "extapi/data/ehub" then
                ehub_data = data; ehub_ts = now
                -- Latch ehub.soc whenever it's present (it's only emitted
                -- on a slow cadence, ~1 in 9 messages on the firmware we
                -- tested). Once latched, it stays the authoritative SoC
                -- until EHUB_SOC_STALE_MS elapses without a refresh.
                local soc_field = data["soc"]
                if soc_field then
                    local v = type(soc_field) == "table" and soc_field.val or soc_field
                    local n = tonumber(v)
                    if n then
                        if n > 1 then n = n / 100 end
                        last_ehub_soc    = n
                        last_ehub_soc_ts = now
                    end
                end
            elseif msg.topic == "extapi/data/eso" then
                local eid = eso_id_of(data)
                eso_data_by_id[eid] = data
                eso_ts_by_id[eid]   = now
            elseif msg.topic == "extapi/data/sso" then
                sso_data = data; sso_ts = now
            elseif msg.topic == "extapi/control/response" then
                -- Track EMS-side ack/nak counters per published cmd.
                -- A NAK is the canary for trouble (`"no available ESOs
                -- detected in system"` preceded the 2026-05-27 brick by
                -- minutes). Log every NAK so operators can correlate
                -- with their command stream; surface the count as a
                -- metric so ops dashboards can alert on rate.
                local status = data["status"]
                if status == "nak" then
                    extapi_nak_count = extapi_nak_count + 1
                    host.log("warn", string.format(
                        "Ferroamp: extapi NAK (transId=%s msg=%s)",
                        tostring(data["transId"] or "?"),
                        tostring(data["msg"] or "?")))
                elseif status == "ack" then
                    extapi_ack_count = extapi_ack_count + 1
                end
            end
        end
    end
    host.emit_metric("extapi_nak_count", extapi_nak_count)
    host.emit_metric("extapi_ack_count", extapi_ack_count)

    -- Drop stale caches so the rest of the poll falls through and
    -- the watchdog catches us when the EnergyHub stops publishing.
    -- Per-topic so a partial outage (e.g. eso lags but ehub flows)
    -- still lets the live channels through.
    if ehub_data and (now - ehub_ts) > STALE_AFTER_MS then
        host.log("warn", "Ferroamp: ehub stale (" .. (now - ehub_ts) .. " ms) — dropping cache")
        ehub_data = nil
        last_ehub_soc    = nil
        last_ehub_soc_ts = 0
    end
    -- Evict per-ESO entries individually so one silent ESO does not
    -- drag the others' contribution out of the aggregate. The freshest
    -- ESO timestamp is then used for the topic-level age metric.
    local eso_ts = 0
    for eid, ts in pairs(eso_ts_by_id) do
        if (now - ts) > STALE_AFTER_MS then
            eso_data_by_id[eid] = nil
            eso_ts_by_id[eid]   = nil
        elseif ts > eso_ts then
            eso_ts = ts
        end
    end
    if sso_data and (now - sso_ts) > STALE_AFTER_MS then
        sso_data = nil
    end

    -- Diagnostics: per-topic age into the long-format TS DB so
    -- operators can see partial outages directly in the metric
    -- browser. Reported as "0" when never seen yet (ts = 0).
    host.emit_metric("ehub_age_ms", ehub_ts == 0 and 0 or (now - ehub_ts))
    host.emit_metric("eso_age_ms",  eso_ts  == 0 and 0 or (now - eso_ts))
    host.emit_metric("sso_age_ms",  sso_ts  == 0 and 0 or (now - sso_ts))

    --------------------------------------------------------------------------
    -- Meter (grid connection point)
    --------------------------------------------------------------------------
    if ehub_data then
        local pext     = extract_val(ehub_data, "pext")     -- per-phase grid power (W)
        local gridfreq = extract_val(ehub_data, "gridfreq") -- grid frequency (Hz)
        local ul       = extract_val(ehub_data, "ul")       -- per-phase voltage (V)
        -- iext = per-phase GRID current at the service-entrance CTs, the
        -- same source pext is derived from. NOT il (which is inverter AC
        -- current and misses any load not routed through the Ferroamp
        -- inverter, e.g. an EV charger on a separate breaker — that mix
        -- made the fuse bars under-read by the EV share of total import).
        local iext     = extract_val(ehub_data, "iext")     -- per-phase grid current (A)
        -- 3-phase energy totals in mJ
        local wextconsq3p = extract_val(ehub_data, "wextconsq3p") -- total import mJ
        local wextprodq3p = extract_val(ehub_data, "wextprodq3p") -- total export mJ

        local meter = {}

        -- Grid power: negative = exporting, positive = importing
        meter.w    = sum_phases(pext)
        meter.l1_w = phase_val(pext, "L1")
        meter.l2_w = phase_val(pext, "L2")
        meter.l3_w = phase_val(pext, "L3")

        -- Grid frequency
        if gridfreq then
            meter.hz = tonumber(gridfreq) or 0
        end

        -- Per-phase voltage
        meter.l1_v = phase_val(ul, "L1")
        meter.l2_v = phase_val(ul, "L2")
        meter.l3_v = phase_val(ul, "L3")

        -- Per-phase grid current (from service-entrance CTs, consistent
        -- with pext above — previously read il by mistake, which is
        -- inverter AC current).
        meter.l1_a = phase_val(iext, "L1")
        meter.l2_a = phase_val(iext, "L2")
        meter.l3_a = phase_val(iext, "L3")

        -- Energy counters (mJ → Wh)
        if wextconsq3p then
            meter.import_wh = mj_to_wh(wextconsq3p)
        end
        if wextprodq3p then
            meter.export_wh = mj_to_wh(wextprodq3p)
        end

        host.emit("meter", meter)
        -- Diagnostics: long-format TS DB
        if meter.l1_w then host.emit_metric("meter_l1_w", meter.l1_w) end
        if meter.l2_w then host.emit_metric("meter_l2_w", meter.l2_w) end
        if meter.l3_w then host.emit_metric("meter_l3_w", meter.l3_w) end
        if meter.l1_v then host.emit_metric("meter_l1_v", meter.l1_v) end
        if meter.l2_v then host.emit_metric("meter_l2_v", meter.l2_v) end
        if meter.l3_v then host.emit_metric("meter_l3_v", meter.l3_v) end
        if meter.l1_a then host.emit_metric("meter_l1_a", meter.l1_a) end
        if meter.l2_a then host.emit_metric("meter_l2_a", meter.l2_a) end
        if meter.l3_a then host.emit_metric("meter_l3_a", meter.l3_a) end
        if meter.hz   then host.emit_metric("grid_hz",    meter.hz)   end

        local state = extract_val(ehub_data, "state")
        if state then
            local sn = tonumber(state) or 0
            host.emit_metric("ehub_state", sn)
            -- EnergyHub "Fault Mode" = ehub.state bit 15 (0x8000) set (e.g.
            -- 0x8030); normal operating states (0x1001 / 0x1101) don't have it.
            -- In Fault Mode the hub opens its PV + battery relays — it keeps
            -- publishing telemetry but cannot actuate. Flag a device fault so
            -- the dispatcher + MPC exclude it instead of commanding a dead
            -- battery (whose un-delivered power would silently become grid
            -- import) and so the dashboard shows "fault", not "ok". Cleared
            -- automatically when the hub recovers (state bit 15 clears).
            -- (Lua 5.1 has no bitops; isolate bit 15 arithmetically.)
            local faultMode = (math.floor(sn / 32768) % 2) == 1
            host.set_device_fault(faultMode,
                faultMode and ("EnergyHub Fault Mode (ehub state " .. tostring(sn) .. ")") or "")
        end
    end

    --------------------------------------------------------------------------
    -- PV (solar generation)
    --------------------------------------------------------------------------
    if ehub_data or sso_data then
        local ppv = choose_power(extract_val(ehub_data, "ppv"), sso_power(sso_data))
        if ppv then
            -- Negate: Ferroamp reports PV as positive, convention requires negative
            host.emit("pv", { w = -ppv })
        end
    end

    --------------------------------------------------------------------------
    -- Battery  (aggregated across N ESOs)
    --------------------------------------------------------------------------
    if not SKIP_BATTERY and (ehub_data or next(eso_data_by_id)) then
        -- Walk every live ESO once and accumulate the aggregate.
        -- Power is summed (parallel DC strings each push their own
        -- current onto the inverter); voltage / SoC / dc-link / temp
        -- are averaged because cells across ESOs run in lock-step in
        -- a healthy cluster and we want a single representative number
        -- for the dashboard. Cumulative Wh counters are summed so the
        -- battery's lifetime production/consumption reflects all units.
        local pbat_sum = 0
        local pbat_has_any = false
        local v_sum, v_n   = 0, 0
        local a_sum, a_n   = 0, 0
        local soc_sum, soc_n = 0, 0
        local soc_wsum, cap_sum = 0, 0  -- capacity-weighted SoC accumulators
        local udc_sum, udc_n = 0, 0
        local wprod_sum, wprod_n = 0, 0
        local wcons_sum, wcons_n = 0, 0
        local relay_worst, fault_worst = nil, nil
        local n_eso = 0
        local n_discharge_capable, n_charge_capable = 0, 0
        local eso_pbat = {}  -- per-ESO ubat*ibat (+=discharge, -=charge) for the active detector

        -- Fallback weight for ESOs not listed in ESO_CAPACITY_KWH: the
        -- mean of the configured weights. Picking the mean (not 0, not 1)
        -- means a partial config doesn't silently exclude unknown units
        -- from the weighted aggregate. If the user lists zero ESOs we
        -- never enter the weighted branch — `cap_sum > 0` gates it.
        local default_cap = nil
        do
            local s, n = 0, 0
            for _, kwh in pairs(ESO_CAPACITY_KWH) do s = s + kwh; n = n + 1 end
            if n > 0 then default_cap = s / n end
        end

        for id, d in pairs(eso_data_by_id) do
            n_eso = n_eso + 1
            local u = tonumber(extract_val(d, "ubat"))
            local i = tonumber(extract_val(d, "ibat"))
            if u and i then
                pbat_sum = pbat_sum + (u * i)
                pbat_has_any = true
                eso_pbat[#eso_pbat + 1] = u * i  -- +discharge / -charge (per-ESO)
            end
            if u then v_sum = v_sum + u; v_n = v_n + 1 end
            if i then a_sum = a_sum + i; a_n = a_n + 1 end

            local soc = tonumber(extract_val(d, "soc"))
            if soc then
                if soc > 1 then soc = soc / 100 end
                soc_sum = soc_sum + soc; soc_n = soc_n + 1
                local cap = ESO_CAPACITY_KWH[tostring(id)] or default_cap
                if cap then
                    soc_wsum = soc_wsum + soc * cap
                    cap_sum  = cap_sum  + cap
                end
                if soc >= DISCHARGE_FLOOR_SOC then
                    n_discharge_capable = n_discharge_capable + 1
                end
                -- SoC-capable counts are used only for the "every unit
                -- floored/ceilinged → idle" guard in driver_command. The
                -- magnitude up-scale is handled by the delivery-ratio loop
                -- (see note near MAX_DISPATCH_SCALE), not a per-unit vote.
                if soc <= CHARGE_CEIL_SOC then
                    n_charge_capable = n_charge_capable + 1
                end
            else
                -- Missing SoC: treat as capable in both directions. The
                -- alternative (skip from capable counts but keep in
                -- n_eso) inflates scale and overdelivers; counting as
                -- capable only under-scales by one slot in the rare
                -- transient where an ESO is genuinely floored AND its
                -- soc field went missing in the same payload.
                n_discharge_capable = n_discharge_capable + 1
                n_charge_capable    = n_charge_capable    + 1
            end
            local udc = tonumber(extract_val(d, "udc"))
            if udc then udc_sum = udc_sum + udc; udc_n = udc_n + 1 end
            local wp = tonumber(extract_val(d, "wbatprod"))
            if wp then wprod_sum = wprod_sum + wp; wprod_n = wprod_n + 1 end
            local wc = tonumber(extract_val(d, "wbatcons"))
            if wc then wcons_sum = wcons_sum + wc; wcons_n = wcons_n + 1 end

            -- relaystatus + faultcode: worst-of so a single faulted
            -- ESO surfaces in the diagnostic, instead of being averaged
            -- away by its still-healthy peers.
            local relay = tonumber(extract_val(d, "relaystatus"))
            if relay then relay_worst = math.max(relay_worst or 0, relay) end
            local fault = tonumber(extract_val(d, "faultcode"))
            if fault then fault_worst = math.max(fault_worst or 0, fault) end
        end

        local battery = nil
        if pbat_has_any then
            -- Prefer per-ESO ubat*ibat because the cell-monitor numbers
            -- update at the ESO's own publish cadence and don't get
            -- rounded through ehub's aggregate. Ferroamp convention:
            -- positive pbat = discharging, negate for site convention
            -- (positive = charging).
            battery = { w = -pbat_sum }
        elseif ehub_data then
            -- No live per-ESO measurement: fall back to ehub.pbat
            -- (also positive = discharging on Ferroamp's side).
            local ehub_pbat = tonumber(extract_val(ehub_data, "pbat"))
            if ehub_pbat then battery = { w = -ehub_pbat } end
        end

        -- Per-ESO delivery detector: count how many units actually delivered
        -- power in the direction of our last on-wire setpoint. A unit at its
        -- SoC limit (full on charge, empty on discharge) delivers ~0 and is
        -- counted out; driver_command then scales N_eso/N_active so the
        -- delivering units cover the stuck unit's even share. Measured from
        -- per-ESO ubat*ibat (+discharge / -charge), not SoC, so a unit just
        -- above the floor that still won't dispatch is correctly excluded.
        -- A unit counts as "active" only if it carries at least half its 1/N
        -- share of the on-wire setpoint. Only measured above the noise floor;
        -- a fresh dispatch / direction flip / idle clears the counts so the
        -- next command starts at 1.0x and re-converges within a tick.
        if (last_control_mode == "charge" or last_control_mode == "discharge")
           and last_commanded_w >= SCALE_FB_MIN_W and n_eso > 0 and #eso_pbat > 0 then
            -- Threshold = half a unit's 1/N share of the COMMANDED power (not
            -- the scaled on-wire value): a delivering unit carries >= 1/N of
            -- commanded (more once scaled), comfortably above this, while a
            -- stuck unit reads ~0. Keying off commanded keeps the threshold
            -- fixed as scale climbs, so it can't exclude real deliverers.
            local thresh = ACTIVE_SHARE_FRAC * (last_commanded_w / n_eso)
            local nc, nd = 0, 0
            for _, p in ipairs(eso_pbat) do
                if p <= -thresh then nc = nc + 1 end   -- charging at/above share
                if p >=  thresh then nd = nd + 1 end   -- discharging at/above share
            end
            last_eso_active_charge    = nc
            last_eso_active_discharge = nd
        elseif last_control_mode == "idle" or last_control_mode == "auto" then
            last_eso_active_charge    = nil
            last_eso_active_discharge = nil
        end
        host.emit_metric("eso_active_charge",    last_eso_active_charge    or 0)
        host.emit_metric("eso_active_discharge", last_eso_active_discharge or 0)

        if battery then
            if v_n   > 0 then battery.v = v_sum / v_n end
            if a_n   > 0 then battery.a = a_sum end           -- sum, not avg: parallel currents add

            -- SoC selection on multi-ESO sites. Priority:
            --   1. ehub.soc — Ferroamp's own system SoC, the same number
            --      their mobile app shows. Cached because it's only
            --      published on a slow cadence (~1 in 9 ehub messages on
            --      the firmware we tested). When fresh this is the
            --      authoritative reading; matches the app to within
            --      rounding on heterogeneous clusters.
            --   2. Capacity-weighted mean — when ehub.soc is stale or
            --      absent and the user has configured per-ESO capacities
            --      (eso_capacity_kwh in YAML). The physically-correct
            --      total-stored / total-capacity number.
            --   3. Arithmetic mean of per-ESO soc — historical behaviour,
            --      always emitted as bat_soc_eso_mean for observability.
            local soc_eso_mean
            if soc_n > 0 then soc_eso_mean = soc_sum / soc_n end
            local soc_weighted
            if cap_sum > 0 then soc_weighted = soc_wsum / cap_sum end
            local soc_ehub
            if last_ehub_soc ~= nil and (now - last_ehub_soc_ts) <= EHUB_SOC_STALE_MS then
                soc_ehub = last_ehub_soc
            end
            if soc_ehub then
                battery.soc = soc_ehub
            elseif soc_weighted then
                battery.soc = soc_weighted
            elseif soc_eso_mean then
                battery.soc = soc_eso_mean
            end

            if wprod_n > 0 then battery.discharge_wh = mj_to_wh(wprod_sum) end
            if wcons_n > 0 then battery.charge_wh    = mj_to_wh(wcons_sum) end

            -- Per-direction capability for the dispatcher's reallocation
            -- (docs/.../capability-aware-battery-reallocation). Mirrors the
            -- eso_*_capable counts that already drive our own EHub dispatch
            -- scaling: when every ESO is floored/ceilinged the pack can't
            -- move that way this cycle, so the dispatcher should hand its
            -- share to a capable sibling instead of commanding a setpoint
            -- driver_command would only idle. Absent on legacy fields →
            -- the dispatcher assumes capable, so this is purely additive.
            battery.discharge_capable = n_discharge_capable > 0
            battery.charge_capable    = n_charge_capable > 0

            host.emit("battery", battery)
            if battery.v then host.emit_metric("battery_dc_v", battery.v) end
            if battery.a then host.emit_metric("battery_dc_a", battery.a) end
            if soc_ehub     then host.emit_metric("bat_soc_ehub",     soc_ehub)     end
            if soc_eso_mean then host.emit_metric("bat_soc_eso_mean", soc_eso_mean) end
            if soc_weighted then host.emit_metric("bat_soc_weighted", soc_weighted) end

            if n_eso > 0 then
                host.emit_metric("eso_count", n_eso)
                host.emit_metric("eso_discharge_capable", n_discharge_capable)
                host.emit_metric("eso_charge_capable",    n_charge_capable)
                if udc_n > 0     then host.emit_metric("eso_dc_link_v",   udc_sum / udc_n) end
                if relay_worst ~= nil then host.emit_metric("eso_relaystatus", relay_worst) end
                if fault_worst ~= nil then host.emit_metric("eso_faultcode",   fault_worst) end
            end
        end

        -- Publish counts to module state so driver_command can scale.
        -- Stamp the timestamp too — driver_command must refuse to scale
        -- on stale data, otherwise a partial broker stall (some ESOs
        -- evicted, others not) leaves an inflated scale persisting
        -- between polls.
        last_eso_count             = n_eso
        last_eso_discharge_capable = n_discharge_capable
        last_eso_charge_capable    = n_charge_capable
        last_eso_counts_ms         = now
    end

    if sso_data then
        local sso_udc = extract_val(sso_data, "udc")
        if sso_udc then host.emit_metric("sso_dc_link_v", tonumber(sso_udc) or 0) end
        local sso_upv = extract_val(sso_data, "upv")
        local upv_n = tonumber(sso_upv) or 0
        if sso_upv then host.emit_metric("sso_pv_v", upv_n) end
        local sso_ipv = extract_val(sso_data, "ipv")
        local ipv_n = tonumber(sso_ipv) or 0
        if sso_ipv then host.emit_metric("sso_pv_a", ipv_n) end
        local sso_relay = extract_val(sso_data, "relaystatus")
        local relay_n = tonumber(sso_relay) or 0
        if sso_relay then host.emit_metric("sso_relaystatus", relay_n) end
        local sso_fault = extract_val(sso_data, "faultcode")
        local fault_n = tonumber(sso_fault) or 0
        if sso_fault then host.emit_metric("sso_faultcode", fault_n) end

        -- Stuck-pplim self-healing watchdog. The 2026-05-27 incident
        -- left the SSO at upv≈500V + ipv=0 + faultcode=0 + relay=1
        -- (paneler aktiva, ingen MPPT, ingen hårdvarufel) — the
        -- signature of a sticky `pplim arg=0` lock. Recovery via portal
        -- took 30+ minutes. If the operator has opted in by setting
        -- `pplim_release_w`, the driver now detects the signature and
        -- auto-publishes a release (`pplim arg=PPLIM_RELEASE_W`) after
        -- STUCK_PV_AFTER_MS continuous, throttled by a cooldown so a
        -- persistent issue can't loop us into command-spam.
        local stuck = (upv_n > STUCK_PV_DC_V_THRESHOLD)
                  and (ipv_n == 0)
                  and (fault_n == 0)
                  and (relay_n == 1)
        if stuck then
            if stuck_pv_since_ms < 0 then
                stuck_pv_since_ms = now
            elseif (now - stuck_pv_since_ms) > STUCK_PV_AFTER_MS then
                local cool_ok = stuck_pv_last_recovery_ms < 0
                            or (now - stuck_pv_last_recovery_ms) > STUCK_PV_RECOVERY_COOLDOWN_MS
                if PPLIM_RELEASE_W > 0 and cool_ok then
                    local payload = string.format(
                        '{"transId":"stuck-pv-recover","cmd":{"name":"pplim","arg":%d}}',
                        PPLIM_RELEASE_W)
                    local err = host.mqtt_publish("extapi/control/request", payload)
                    if not err then
                        stuck_pv_last_recovery_ms = now
                        stuck_pv_recovery_count   = stuck_pv_recovery_count + 1
                        host.log("warn", string.format(
                            "Ferroamp: stuck-pplim detected (upv=%.1fV, ipv=0A for %d min) — auto-published pplim arg=%d to recover",
                            upv_n, math.floor((now - stuck_pv_since_ms) / 60000), PPLIM_RELEASE_W))
                    end
                elseif PPLIM_RELEASE_W <= 0 and cool_ok then
                    -- Log-only path: operator hasn't opted in. Reuse the
                    -- cooldown so we don't spam every poll.
                    stuck_pv_last_recovery_ms = now
                    host.log("warn", string.format(
                        "Ferroamp: stuck-pplim signature for %d min (upv=%.1fV, ipv=0A) — set config.pplim_release_w to enable auto-recovery",
                        math.floor((now - stuck_pv_since_ms) / 60000), upv_n))
                end
            end
        else
            stuck_pv_since_ms = -1
        end
        host.emit_metric("stuck_pv_recovery_count", stuck_pv_recovery_count)
    end

    return 1000
end

----------------------------------------------------------------------------
-- Control
----------------------------------------------------------------------------

-- Control: Ferroamp External API
-- Reference: https://github.com/henricm/ha-ferroamp
-- Topic: extapi/control/request
-- Commands:
--   {"transId":"...","cmd":{"name":"charge","arg":<watts>}}    — force charge (arg always positive)
--   {"transId":"...","cmd":{"name":"discharge","arg":<watts>}} — force discharge (arg always positive)
--   {"transId":"...","cmd":{"name":"auto"}}                    — return to auto mode
-- EMS convention: positive power_w = charge, negative = discharge
function driver_command(action, power_w, cmd)
    if action == "init" then
        return true
    elseif action == "battery" then
        local now = host.millis()
        local tid = "ems-" .. tostring(now)
        -- Up-scale the on-wire setpoint so the units that ARE delivering cover
        -- the even share of any unit stuck at 0 (the EHub splits the setpoint
        -- evenly across all ESOs). Inactive units are detected by actual power
        -- delivery, not SoC (see the active-detector note near
        -- MAX_DISPATCH_SCALE). The SoC-capable count is used ONLY to detect
        -- "every unit floored/ceilinged → idle". We act only when the per-ESO
        -- snapshot is fresh (<= STALE_AFTER_MS) so a broker stall can't scale
        -- off a stale snapshot.
        --
        -- Reset the active counts on a direction flip so each direction
        -- re-converges from 1.0x instead of inheriting the other's count.
        if (power_w > 0 and last_control_mode ~= "charge") or
           (power_w < 0 and last_control_mode ~= "discharge") then
            last_eso_active_charge    = nil
            last_eso_active_discharge = nil
        end
        local on_wire_w = power_w
        local scale     = 1.0
        local fresh     = last_eso_counts_ms >= 0
                          and (now - last_eso_counts_ms) <= STALE_AFTER_MS
        if fresh and last_eso_count > 0 and power_w ~= 0 then
            local capable
            if power_w > 0 then capable = last_eso_charge_capable
            else                 capable = last_eso_discharge_capable end
            if capable == 0 then
                -- Every unit floored/ceilinged for this direction — publish
                -- idle rather than command something nothing can honour.
                host.log("warn", string.format(
                    "Ferroamp: all %d ESOs at SoC limit for requested %d W — idling",
                    last_eso_count, math.floor(power_w)))
                host.emit_metric("eso_dispatch_scale_x1000", 0)
                host.emit_metric("eso_dispatch_commanded_w", 0)
                last_on_wire_w = 0
                last_commanded_w = 0
                -- Re-publish every tick (see the zero branch below): a
                -- one-shot idle lets the EHub's forced mode expire and
                -- revert to autonomous charging from grid.
                return publish_idle(tid)
            end
            -- Scale by N_eso / N_active in the commanded direction, where
            -- N_active is how many units actually DELIVERED power last tick
            -- (measured, not SoC). Same detector both directions: a full unit
            -- on charge and an empty unit on discharge both read "0 delivered"
            -- → excluded → the rest cover their share. The active units split
            -- the commanded power exactly, so the pack delivers the commanded
            -- total and never more (scale 1.0 when all deliver). nil active
            -- (fresh dispatch / direction flip / sub-noise command) → 1.0x and
            -- re-measure next tick.
            local active
            if power_w > 0 then active = last_eso_active_charge
            else                 active = last_eso_active_discharge end
            if active ~= nil and active > 0 and active < last_eso_count then
                scale = last_eso_count / active
                if scale > MAX_DISPATCH_SCALE then scale = MAX_DISPATCH_SCALE end
            end
            on_wire_w = power_w * scale
        end
        last_on_wire_w = math.abs(on_wire_w)   -- scaled setpoint actually sent
        last_commanded_w = math.abs(power_w)   -- dispatch command — the active threshold base
        host.emit_metric("eso_dispatch_scale_x1000",  math.floor(scale * 1000 + 0.5))
        host.emit_metric("eso_dispatch_commanded_w",  math.floor(power_w))
        if power_w > 0 then
            -- Charge: use "charge" command with positive watts
            local payload = string.format(
                '{"transId":"%s","cmd":{"name":"charge","arg":%d}}',
                tid, math.floor(on_wire_w)
            )
            local err = host.mqtt_publish("extapi/control/request", payload)
            if not err then last_control_mode = "charge" end
            return err
        elseif power_w < 0 then
            -- Discharge: use "discharge" command with positive watts
            local payload = string.format(
                '{"transId":"%s","cmd":{"name":"discharge","arg":%d}}',
                tid, math.floor(math.abs(on_wire_w))
            )
            local err = host.mqtt_publish("extapi/control/request", payload)
            if not err then last_control_mode = "discharge" end
            return err
        else
            -- Zero: force idle at 0 W, re-published EVERY tick. The EHub's
            -- forced-mode command (discharge arg=0) EXPIRES if not
            -- refreshed — a one-shot idle let the EHub revert to autonomous
            -- self-consumption and charge the battery ~2.6 kW from the GRID
            -- while FtW believed it was idling (observed on Stefan's site
            -- 2026-06-10: dispatch target 0, battery charging 2.6 kW anyway,
            -- FtW silent on the control topic for 12 s). Re-publishing each
            -- tick keeps the EHub under our control, exactly as the
            -- charge/discharge branches above already refresh their setpoints.
            return publish_idle(tid)
        end
    elseif action == "curtail" then
        -- Ferroamp's extapi treats `pplim arg=0` as "limit PV output to
        -- 0 W" — same wire bytes as a release would have, opposite
        -- semantics. A curtail request that resolves to ≤ 0 W therefore
        -- locks the inverter at zero PV until the operator manually
        -- clears pplim from the Ferroamp portal or power-cycles the
        -- EnergyHub. Refuse the dangerous request — log a warning so
        -- the operator sees the regression upstream (dispatch sent a
        -- zero target for a curtail-capable driver, which shouldn't
        -- happen because the dispatcher filters out drivers with
        -- |PV| == 0 before allocating). 2026-05-27 incident: a 0-share
        -- allocation through this path bricked the live SE4 site for
        -- 30+ minutes; needed a Ferroamp portal reset to recover.
        local watts = math.floor(math.abs(power_w))
        if watts <= 0 then
            host.log("warn", string.format(
                "Ferroamp: refusing pplim=0 (sticky lock on this firmware); upstream wanted curtail to %d W",
                math.floor(power_w)))
            return nil
        end
        local payload = string.format(
            '{"transId":"ems","cmd":{"name":"pplim","arg":%d}}', watts)
        return host.mqtt_publish("extapi/control/request", payload)
    elseif action == "curtail_disable" then
        -- DO NOT send `pplim arg=0` here. On Ferroamp's extapi that is
        -- "limit to 0 W", not "release the limit", and it sticks until
        -- portal reset. The safe release is to publish a pplim equal
        -- to the operator-declared maximum (or skip the publish when
        -- no max is configured, leaving whatever pplim Ferroamp last
        -- received in place — operator can clear it from the portal).
        -- See drivers/ferroamp.lua docs section for pplim_release_w.
        if PPLIM_RELEASE_W > 0 then
            local payload = string.format(
                '{"transId":"ems","cmd":{"name":"pplim","arg":%d}}',
                PPLIM_RELEASE_W)
            return host.mqtt_publish("extapi/control/request", payload)
        end
        host.log("info",
            "Ferroamp: curtail_disable skipped (set config.pplim_release_w to enable; default behaviour avoids the sticky pplim=0 trap)")
        return nil
    elseif action == "deinit" then
        return publish_auto("ems")
    end
    return false
end

function driver_default_mode()
    publish_auto("watchdog")
end

function driver_cleanup()
    -- Leave the EnergyHub in autonomous self-consumption when the EMS
    -- stops or the driver hot-reloads. Otherwise the last forced
    -- charge/discharge reference can remain visible in the Ferroamp app.
    pcall(publish_auto, "cleanup")
    ehub_data = nil
    last_ehub_soc    = nil
    last_ehub_soc_ts = 0
    eso_data = nil
    sso_data = nil
    last_eso_count             = 0
    last_eso_discharge_capable = 0
    last_eso_charge_capable    = 0
    last_eso_counts_ms         = -1
    last_eso_active_charge     = nil
    last_eso_active_discharge  = nil
    last_on_wire_w             = 0
end
