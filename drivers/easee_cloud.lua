-- Easee Cloud EV Charger Driver
-- Emits: EV
-- Protocol: HTTPS (Easee Cloud REST API)
--
-- Authenticates with the user's Easee account (email + password from
-- config), polls charger state every 5 seconds, and emits DerEV
-- readings so the dispatch clamp keeps home batteries from feeding
-- the car.
--
-- Config example:
--   drivers:
--     - name: easee
--       lua: drivers/easee_cloud.lua
--       capabilities:
--         http:
--           allowed_hosts: ["api.easee.com"]
--       config:
--         email: "user@example.com"
--         password: "secret"
--         serial: "EHHZBKPF"    # optional — auto-detected if omitted

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "easee-cloud",
  name         = "Easee Cloud",
  manufacturer = "Easee",
  version      = "1.0.1",
  protocols    = { "http" },
  capabilities = { "ev" },
  description  = "Easee Home/Charge via Cloud REST API. No local protocol needed.",
  homepage     = "https://easee.com",
  http_hosts   = { "api.easee.com" },
  authors      = { "FTW contributors" },
  tested_models = { "Home", "Charge" },
  verification_status = "production",
  verified_by = { "frahlg@homelab-rpi:2d", "erikarenhill@fortytwo:1d" },
  verified_at = "2026-04-18",
  verification_notes = "Observations API + lifecycle commands exercised against an Easee Home charger. Session state, op_mode labels, charge/pause/resume all verified.",
}

PROTOCOL = "http"

local BASE_URL = "https://api.easee.com/api"
local access_token = nil
local refresh_token = nil
local token_expires_at = 0   -- millis
local charger_serial = nil
local phases = 3   -- populated from config.phases (if present) in driver_init
local last_sent_phases = nil   -- tracks the last phaseMode posted to Easee
local last_phase_change_ms = 0 -- monotonic ms of the last phase flip (hysteresis)

-- After every phaseMode change, Easee resets dynamicChargerCurrent to
-- the static maxChargerCurrent value (16 A on a typical 16 A install).
-- That resets sticks for ~5–10 s on the cloud side before our follow-
-- up dynamicChargerCurrent write propagates back. To bound that
-- window, we (a) re-write dynamicChargerCurrent on the next driver_poll
-- after a phaseMode change, and (b) clamp maxChargerCurrent at init
-- when an operator config field requests it (see driver_init below).
local pending_amp_resend = false
local pending_amp_resend_at_ms = 0
local last_amps_set = nil

-- paused_state tracks whether the LAST command we successfully sent was
-- ev_pause. Easee's REST API has no way to query "are you currently
-- in user-pause" — op_mode 2 ("awaiting start") covers both "paused"
-- and "plugged in but car hasn't started a session yet". Without this
-- flag, after a controller pause + re-offer, we'd write
-- dynamicChargerCurrent=6 successfully but the contactor stays open
-- because we never sent resume_charging. We saw this in the field as
-- "easee_a=6, easee_chg=false, ev_w=0, reason=100/52" stuck states.
local paused_state = false

-- command_stalled_since_ms tracks when we last wrote a non-zero amps
-- offer that did NOT translate into actual charging. Used to surface
-- a derived diagnostic for the controller / UI so a stuck Easee
-- (firmware error, EV-side reject) is observable rather than silent.
local command_stalled_since_ms = 0

-- Easee charger physical limits. These are the manufacturer's hard
-- bounds, not operator preferences — `dynamicChargerCurrent` written
-- below the minimum is silently rejected by the unit (returns success
-- but no current actually flows), and above the max the cloud returns
-- a 4xx. The min applies when amps > 0; commanding exactly 0 always
-- pauses cleanly.
local EASEE_MIN_A = 6
local EASEE_MAX_A = 32

-- pick_phases is the driver-level phase decision: given the requested
-- charging power, the per-phase fuse ceiling, the operator's mode
-- preference, and the time since the last flip, return how many
-- phases (1 or 3) to actually commit to this command.
--
--   * mode "1p" / "3p": locked, no hysteresis
--   * mode "auto" or empty + missing fuse data: legacy 3Φ fallback
--     (preserves pre-switching behaviour for sites that don't pass
--     fuse data in the cmd)
--   * mode "auto" with fuse data: pick 1Φ while wantW is below both
--     the configured split and the first deliverable 3Φ step. This
--     avoids the physical dead zone between a 16 A 1Φ ceiling
--     (3.68 kW at 230 V) and Easee's 6 A/phase 3Φ minimum (4.14 kW):
--     the 1Φ command saturates safely instead of a 3Φ command mapping
--     to 0 A. Hysteresis suppresses
--     a flip when less than `hold_s` seconds have passed since the
--     last change — Easee's contactor + cloud-API round-trip is
--     ~5-10 s, so flapping at every controller tick would burn the
--     contactor and never deliver useful power.
local function pick_phases(mode, want_w, voltage, max_a_per_phase, split_w, hold_s, now_ms)
    local locked = mode
    if locked == "1p" then return 1 end
    if locked ~= "auto" then return 3 end -- "" / "3p" / unknown

    -- Effective split: operator override → site fuse → 230 V × 16 A default.
    local s = split_w
    if (s == nil) or (s <= 0) then
        if voltage and voltage > 0 and max_a_per_phase and max_a_per_phase > 0 then
            s = voltage * max_a_per_phase
        else
            s = 3680
        end
    end

    -- Never switch to 3Φ before Easee can deliver its minimum current
    -- on every phase. A lower phase_split_w (explicit or fuse-derived)
    -- would otherwise turn e.g. 3.6–4.0 kW into 5 A/phase, which
    -- per_phase_amps must correctly pause as 0 A. Staying on 1Φ may
    -- saturate at the fuse ceiling for this narrow band, but it keeps
    -- useful current flowing without exceeding the installation limit.
    local v = (voltage and voltage > 0) and voltage or 230
    local min_3p_w = v * 3 * EASEE_MIN_A
    if s < min_3p_w then s = min_3p_w end

    local desired = (want_w < s) and 1 or 3

    -- Hysteresis: once we've committed to a phase count, hold it for
    -- at least `hold_s` seconds before flipping the other way.
    if last_sent_phases == nil then
        return desired
    end
    if desired == last_sent_phases then
        return desired
    end
    -- Default hold = 90 s. Field-measured against an Easee Home: a full
    -- phaseMode flip + EV ramp to steady state takes 60–90 s; a 60 s
    -- hold could let a flap-prone wantW trigger another transition
    -- mid-ramp. 90 s gives one full ramp cycle before another flip.
    local hold = (hold_s and hold_s > 0) and hold_s or 90
    if (now_ms - last_phase_change_ms) < (hold * 1000) then
        return last_sent_phases
    end
    return desired
end

-- per_phase_amps converts the requested W into the dynamicChargerCurrent
-- value Easee expects. Voltage and per-phase fuse ceiling come from
-- the cmd (controller-supplied) so a 240 V or 220 V mains is handled
-- without hard-coding 230. The result is rounded (not floored) and
-- clamped to [0, EASEE_MAX_A] AND to the per-phase fuse ceiling so
-- the breaker stays safe even if the controller sent a value above
-- what the fuse tolerates.
local function per_phase_amps(power_w, voltage, p, max_a_per_phase)
    local v = (voltage and voltage > 0) and voltage or 230
    local raw = ((power_w or 0) / v / p) + 0.5
    local amps = math.floor(raw)
    if amps < 0 then amps = 0 end
    if amps > EASEE_MAX_A then amps = EASEE_MAX_A end
    if max_a_per_phase and max_a_per_phase > 0 and amps > max_a_per_phase then
        amps = math.floor(max_a_per_phase) -- never breach the fuse
    end
    -- Below Easee's hardware minimum, pause cleanly instead of leaving
    -- the charger in "I requested 4 A but nothing flows" limbo.
    if amps > 0 and amps < EASEE_MIN_A then amps = 0 end
    return amps
end

-- Easee error bodies have historically echoed submitted form data
-- (credentials, tokens). Strip the body and keep only the status prefix
-- so nothing sensitive ever lands in the driver log.
local function redact_http_err(err)
    if err == nil then return "request failed" end
    return tostring(err):match("^(HTTP %d+)") or "request failed"
end

-- ---- Auth helpers ----

local function login(email, password)
    local body = host.json_encode({userName = email, password = password})
    local resp, err = host.http_post(BASE_URL .. "/accounts/login", body)
    if err then
        host.log("error", "Easee login failed: " .. redact_http_err(err))
        return false
    end
    local data = host.json_decode(resp)
    if not data or not data.accessToken then
        host.log("error", "Easee login: no accessToken in response")
        return false
    end
    access_token = data.accessToken
    refresh_token = data.refreshToken
    -- expiresIn is seconds; convert to absolute millis
    local expires_in = data.expiresIn or 3600
    token_expires_at = host.millis() + (expires_in * 1000) - 60000 -- refresh 1 min early
    host.log("info", "Easee: logged in, token expires in " .. expires_in .. "s")
    return true
end

local function do_refresh()
    if not access_token or not refresh_token then return false end
    local body = host.json_encode({
        accessToken = access_token,
        refreshToken = refresh_token
    })
    local resp, err = host.http_post(BASE_URL .. "/accounts/refresh_token", body)
    if err then
        host.log("warn", "Easee token refresh failed: " .. redact_http_err(err))
        return false
    end
    local data = host.json_decode(resp)
    if not data or not data.accessToken then
        host.log("warn", "Easee refresh: no accessToken, will re-login")
        return false
    end
    access_token = data.accessToken
    refresh_token = data.refreshToken
    local expires_in = data.expiresIn or 3600
    token_expires_at = host.millis() + (expires_in * 1000) - 60000
    host.log("debug", "Easee: token refreshed")
    return true
end

local function ensure_auth(email, password)
    if access_token and host.millis() < token_expires_at then
        return true
    end
    -- Try refresh first, fall back to full login
    if access_token and do_refresh() then
        return true
    end
    return login(email, password)
end

local function auth_headers()
    return {Authorization = "Bearer " .. (access_token or "")}
end

-- ---- API helpers ----

local function get_chargers()
    local resp, err = host.http_get(BASE_URL .. "/chargers", auth_headers())
    if err then return nil, err end
    return host.json_decode(resp), nil
end

-- Observation IDs (from developer.easee.com/docs/charger-observation-ids)
local OBS_DYN_CURRENT     = 48
local OBS_REASON_NO_CUR   = 96
local OBS_CABLE_LOCKED    = 103
local OBS_OP_MODE         = 109
local OBS_TOTAL_POWER     = 120
local OBS_SESSION_ENERGY  = 121
local OBS_LIFETIME_ENERGY = 124
local OBS_CURRENT         = 183
local OBS_VOLTAGE         = 194

local OBS_IDS = "48,96,103,109,120,121,124,183,194"

local function get_observations(serial)
    local url = "https://api.easee.com/state/" .. serial .. "/observations?ids=" .. OBS_IDS
    local resp, err = host.http_get(url, auth_headers())
    if err then return nil, err end
    local decoded = host.json_decode(resp)
    if not decoded then return nil, "decode failed" end
    local list = decoded.observations or decoded
    local obs = {}
    for _, item in ipairs(list) do
        if item.id then
            obs[item.id] = tonumber(item.value) or item.value
        end
    end
    return obs, nil
end

-- ---- State mapping ----
-- Easee chargerOpMode (observation 109):
--   1 = disconnected (standby)
--   2 = awaiting start (connected, not charging)
--   3 = charging
--   4 = completed
--   5 = error
--   6 = ready to charge

local OP_MODE_LABELS = {
    [0] = "offline",
    [1] = "disconnected",
    [2] = "awaiting start",
    [3] = "charging",
    [4] = "completed",
    [5] = "error",
    [6] = "ready",
}

-- Easee ReasonForNoCurrent. 0 = charger OK (no reason to surface).
-- Source: developer.easee.com/docs/enumerations
local REASON_LABELS = {
    [0]   = nil, -- no reason
    [1]   = "max circuit current too low",
    [2]   = "dynamic circuit current too low",
    [3]   = "offline fallback circuit current too low",
    [4]   = "circuit fuse too low",
    [5]   = "waiting in queue",
    [6]   = "waiting (other cars fully charged)",
    [7]   = "illegal grid type",
    [8]   = "no current request from primary",
    [9]   = "max dynamic charger current too low",
    [10]  = "phase imbalance",
    [11]  = "equalizer communication lost",
    [25]  = "equalizer dynamic limit too low",
    [26]  = "equalizer static limit too low",
    [27]  = "offline fallback equalizer too low",
    [28]  = "fuse limit reached",
    [29]  = "current limited by equalizer",
    [30]  = "current limited by offline equalizer",
    [50]  = "secondary unit not requesting current",
    [51]  = "max charger current too low",
    [52]  = "max dynamic charger current too low",
    [53]  = "charger disabled",
    [54]  = "pending scheduled charging",
    [55]  = "pending authorization",
    [56]  = "charger in error state",
    [57]  = "erratic EV",
    [75]  = "limited by cable rating",
    [76]  = "limited by schedule",
    [77]  = "limited by charger current",
    [78]  = "limited by dynamic charger current",
    [79]  = "car not drawing current",
    [80]  = "current ramping",
    [81]  = "limited by car",
    -- Easee's public enumeration doesn't define 100. Field observation:
    -- we see it when the EV is the side rejecting the offer (Tesla
    -- charge-on-solar window, charge limit reached, scheduled charging
    -- still active, transient handshake hiccup). Labelling it
    -- "undefined error" misleads operators into chasing a charger
    -- problem when the wallbox is fine.
    [100] = "EV not accepting current",
}

local email, password, configured_max_a

-- read_settings GETs the charger's static config block (phaseMode,
-- maxChargerCurrent, etc.) and surfaces it via the init log so the
-- operator can spot a firmware-locked phaseMode that would silently
-- ignore our writes. Easee's API uses `/config` for reads and
-- `/settings` for writes — they're not symmetric. Returns the
-- decoded table or nil + err string. Active firmware-lock detection
-- (compare requested phaseMode after a write) is a follow-up; this
-- driver's contribution is the diagnostic logging at init.
local function read_settings(serial)
    local resp, err = host.http_get(BASE_URL .. "/chargers/" .. serial .. "/config", auth_headers())
    if err then return nil, redact_http_err(err) end
    local decoded = host.json_decode(resp)
    if decoded == nil then
        return nil, "decode failed (non-JSON response)"
    end
    return decoded, nil
end

-- write_setting POSTs a single key:value into the charger's settings
-- endpoint. Used at init for maxChargerCurrent (one-shot install
-- clamp) and for phaseMode / dynamicChargerCurrent on every command.
local function write_setting(serial, body_table)
    local _, err = host.http_post(
        BASE_URL .. "/chargers/" .. serial .. "/settings",
        host.json_encode(body_table), auth_headers())
    return err
end

function driver_init(config)
    host.set_make("Easee")

    email = config and config.email
    password = config and config.password
    charger_serial = config and config.serial
    -- UI writes "" for an unselected <select>. In Lua "" is truthy, so the
    -- auto-detect branch below would never fire without this normalization.
    if charger_serial == "" then charger_serial = nil end
    if email == "" then email = nil end
    if password == "" then password = nil end

    -- Phases: default to 3 since that's the common European install.
    -- Users on a single-phase service (Easee Home <11 kW) must set
    -- `phases: 1` in config, otherwise amperage math for ev_set_current
    -- is 3x under-requested and can fall below the 6 A Easee minimum
    -- (silently halting the session).
    if config and tonumber(config.phases) then
        local p = math.floor(tonumber(config.phases))
        if p == 1 or p == 2 or p == 3 then phases = p end
    end

    -- Optional `max_charger_current` config: clamps the charger's
    -- static maxChargerCurrent to this value via the settings API at
    -- init. Bounds the post-phaseMode-reset window (Easee resets
    -- dynamicChargerCurrent → maxChargerCurrent on every phase flip;
    -- if the install's maxChargerCurrent is higher than the per-phase
    -- fuse can sustain, the post-reset window can briefly exceed
    -- safe bounds before the next 5 s controller tick claws it back).
    if config and tonumber(config.max_charger_current) then
        local m = tonumber(config.max_charger_current)
        if m > 0 and m <= EASEE_MAX_A then
            configured_max_a = m
        end
    end

    if not email or not password then
        host.log("error", "Easee: email and password required in driver config")
        return
    end

    if not ensure_auth(email, password) then
        host.log("error", "Easee: initial login failed")
        return
    end

    -- Auto-detect serial if not provided
    if not charger_serial then
        local chargers, cerr = get_chargers()
        if cerr or not chargers or #chargers == 0 then
            host.log("error", "Easee: could not list chargers: " .. redact_http_err(cerr))
            return
        end
        charger_serial = chargers[1].id
        host.log("info", "Easee: auto-detected charger " .. tostring(charger_serial))
    end

    host.set_sn(charger_serial)

    -- Probe firmware-side settings so operators know what the charger
    -- itself is committed to BEFORE our first phaseMode write goes out
    -- (silent overrides got us 30 minutes of confusion in the field —
    -- see the PR description for the test session).
    local settings, serr = read_settings(charger_serial)
    if settings then
        local fw_pm = tonumber(settings.phaseMode)
        local fw_max = tonumber(settings.maxChargerCurrent)
        host.log("info", "Easee: firmware settings — phaseMode=" .. tostring(fw_pm) ..
            " (1=1p,2=auto,3=3p), maxChargerCurrent=" .. tostring(fw_max) .. "A")
        -- Apply the operator's max_charger_current clamp if it differs.
        if configured_max_a and fw_max ~= configured_max_a then
            local werr = write_setting(charger_serial, {maxChargerCurrent = configured_max_a})
            if werr == nil then
                host.log("info", "Easee: maxChargerCurrent clamped to " ..
                    tostring(configured_max_a) .. " A (was " .. tostring(fw_max) .. " A)")
            else
                host.log("warn", "Easee: maxChargerCurrent write failed: " ..
                    redact_http_err(werr))
            end
        end
    elseif serr then
        host.log("warn", "Easee: could not read firmware settings: " .. serr)
    end

    host.log("info", "Easee: driver initialized for " .. charger_serial)
end

function driver_poll()
    if not charger_serial or not email then
        return 10000
    end

    if not ensure_auth(email, password) then
        host.log("warn", "Easee: auth failed, skipping poll")
        return 10000
    end

    local obs, err = get_observations(charger_serial)
    if err or not obs then
        host.log("warn", "Easee: observations poll failed: " .. redact_http_err(err))
        return 10000
    end

    local op_mode = obs[OBS_OP_MODE] or 1
    local power_w = (obs[OBS_TOTAL_POWER] or 0) * 1000  -- kW → W
    local session_wh = (obs[OBS_SESSION_ENERGY] or 0) * 1000  -- kWh → Wh
    local connected = (op_mode >= 2 and op_mode <= 6)
    local charging = (op_mode == 3)
    -- op_mode 0 is the sentinel Easee emits when the cloud hasn't heard
    -- from the unit recently. Anything else means the charger itself is
    -- responsive even when no car is plugged in.
    local is_online = (op_mode ~= 0)

    local reason_code = obs[OBS_REASON_NO_CUR]
    local cable_locked = obs[OBS_CABLE_LOCKED]
    if cable_locked ~= nil then cable_locked = (cable_locked == 1 or cable_locked == true) end
    local dyn_current = obs[OBS_DYN_CURRENT]

    -- Derive actual per-phase amps from the live total power +
    -- per-phase voltage (when available) — diagnostic counterpart to
    -- the `max_a` field which only echoes whatever we last wrote.
    -- Operators wanting "is the EV actually drawing what we asked
    -- for?" should compare actual_amps_per_phase against max_a.
    --
    -- Only computed for the canonical 1Φ / 3Φ configurations. Earlier
    -- driver_init permitted config.phases=2 (a misconfigured site,
    -- or a transitional state before the first successful phaseMode
    -- write). Dividing by 2 there would yield a "per-phase" number
    -- that doesn't correspond to any real cabling — better to omit
    -- the field than mislead the operator with a fictitious value.
    local actual_amps_per_phase = nil
    local v_obs = obs[OBS_VOLTAGE]
    if power_w > 0 and (phases == 1 or phases == 3) then
        local vv = (v_obs and v_obs > 0) and v_obs or 230
        actual_amps_per_phase = power_w / vv / phases
    end

    -- command_stalled: true when we've been offering >0 A for >30 s but
    -- the charger isn't drawing AND Easee is reporting an EV-side or
    -- "max dynamic too low" reason. Lets the controller / UI tell the
    -- difference between "EV declined" (legitimate, e.g. Tesla SoC
    -- limit reached) and "wallbox accepted but contactor stuck"
    -- (needs operator attention). Note: connected==true is required so
    -- a disconnected cable doesn't latch the flag.
    local command_stalled = false
    local now = host.millis()
    local stalled_reason = (reason_code == 52) or (reason_code == 53) or (reason_code == 100)
    if connected and (last_amps_set or 0) > 0 and not charging and stalled_reason then
        if command_stalled_since_ms == 0 then
            command_stalled_since_ms = now
        elseif (now - command_stalled_since_ms) >= 30000 then
            command_stalled = true
        end
    else
        command_stalled_since_ms = 0
    end

    host.emit("ev", {
        w                       = power_w,
        connected               = connected,
        charging                = charging,
        session_wh              = session_wh,
        op_mode                 = op_mode,                     -- 1=disc,2=awaiting,3=charging,4=completed,5=error,6=ready
        state_label             = OP_MODE_LABELS[op_mode] or "unknown",
        reason_no_current       = reason_code,                 -- int: 0=ok; why NOT drawing current
        reason_no_current_label = reason_code and REASON_LABELS[reason_code], -- nil if 0/ok, string otherwise
        is_online               = is_online,
        cable_locked            = cable_locked,
        max_a                   = dyn_current,                 -- last-set dynamic limit (echoes our write, may lag)
        actual_amps_per_phase   = actual_amps_per_phase,       -- live per-phase A derived from totalPower
        phases                  = phases,                      -- our committed phase count (1 or 3)
        command_stalled         = command_stalled,             -- offer>0 but contactor open >30s on stall reason
    })

    -- Defense against Easee's post-phaseMode reset of dynamicChargerCurrent
    -- → maxChargerCurrent. driver_command sets pending_amp_resend=true
    -- right after a successful phaseMode write; on the next poll
    -- (≥ ~5 s later, by which time the cloud has settled) we re-write
    -- our intended amps to overcome the reset. Idempotent — same write
    -- as the controller would issue on its next 5 s tick, just earlier.
    --
    -- Only clear the flag on success. A transient network failure or
    -- 5xx leaves it pending so the NEXT poll retries instead of
    -- silently leaving the charger pinned at maxChargerCurrent. The
    -- controller's own 5 s tick is the safety floor; this just
    -- accelerates convergence in the common case.
    if pending_amp_resend and last_amps_set ~= nil and host.millis() >= pending_amp_resend_at_ms then
        local werr = write_setting(charger_serial, {dynamicChargerCurrent = last_amps_set})
        if werr == nil then
            host.log("info", "Easee: re-asserted dynamicChargerCurrent=" .. tostring(last_amps_set) ..
                " A after phaseMode reset")
            pending_amp_resend = false
        else
            host.log("warn", "Easee: dynamicChargerCurrent re-assert failed (will retry next poll): " ..
                redact_http_err(werr))
        end
    end

    if obs[OBS_VOLTAGE] then
        host.emit_metric("ev_voltage_v", obs[OBS_VOLTAGE])
    end
    if obs[OBS_CURRENT] then
        host.emit_metric("ev_current_a", obs[OBS_CURRENT])
    end
    if obs[OBS_LIFETIME_ENERGY] then
        host.emit_metric("ev_lifetime_kwh", obs[OBS_LIFETIME_ENERGY])
    end
    if dyn_current then
        host.emit_metric("ev_dynamic_current_a", dyn_current)
    end

    return 5000
end

local function post_command(path)
    local _, err = host.http_post(
        BASE_URL .. "/chargers/" .. charger_serial .. path,
        "null", auth_headers())
    return err == nil
end

function driver_command(action, power_w, cmd)
    if not charger_serial or not ensure_auth(email, password) then return false end

    if action == "ev_start" then
        local ok = post_command("/commands/start_charging")
        if ok then paused_state = false end
        return ok
    elseif action == "ev_pause" then
        local ok = post_command("/commands/pause_charging")
        if ok then paused_state = true end
        return ok
    elseif action == "ev_resume" then
        local ok = post_command("/commands/resume_charging")
        if ok then paused_state = false end
        return ok
    elseif action == "ev_set_current" then
        -- Driver-level phase decision: read the operator's preferences
        -- + site fuse from the cmd, decide 1Φ vs 3Φ here based on
        -- the requested W and the voltage we know about. The Go
        -- controller does NOT pick phases; it just allocates power.
        local mode    = (cmd and type(cmd.phase_mode)       == "string") and cmd.phase_mode       or ""
        local split   = (cmd and type(cmd.phase_split_w)    == "number") and cmd.phase_split_w    or 0
        local hold_s  = (cmd and type(cmd.min_phase_hold_s) == "number") and cmd.min_phase_hold_s or 0
        local voltage = (cmd and type(cmd.voltage)          == "number" and cmd.voltage > 0)          and cmd.voltage          or 230
        local max_a   = (cmd and type(cmd.max_amps_per_phase) == "number" and cmd.max_amps_per_phase > 0) and cmd.max_amps_per_phase or nil

        local now_ms = host.millis()
        local requested_phases = pick_phases(mode, power_w or 0, voltage, max_a, split, hold_s, now_ms)

        -- POST phaseMode FIRST when it differs. Easee's settings
        -- endpoint accepts phaseMode = 1 (locked-1p), 2 (auto), 3
        -- (locked-3p). We only ever lock — "auto" is our concern, not
        -- the charger's.
        local phase_changed = false
        if last_sent_phases ~= requested_phases then
            -- A live phase flip (notably 1Φ→3Φ) won't move the Easee
            -- contactor while a session is charging: the phase count is only
            -- latched when a session (re)starts. So on a real mid-session flip
            -- pause first; the phaseMode write reconfigures, and the auto-
            -- resume below (amps > 0 && paused_state) re-closes the contactor
            -- on the new phase count. Operator-confirmed: a manual
            -- pause+resume was the only thing that flipped 1Φ→3Φ. Skip on the
            -- first command of a session (last_sent_phases == nil) — there's no
            -- live contactor to recycle. 2026-05-30.
            if last_sent_phases ~= nil then
                if post_command("/commands/pause_charging") then
                    paused_state = true
                    host.log("info", "Easee: pause to flip phaseMode " ..
                        tostring(last_sent_phases) .. "→" .. tostring(requested_phases))
                end
            end
            local pm_err = write_setting(charger_serial, {phaseMode = requested_phases})
            if pm_err == nil then
                phases = requested_phases
                last_sent_phases = requested_phases
                last_phase_change_ms = now_ms
                phase_changed = true
                host.log("info", "Easee: phaseMode → " .. tostring(requested_phases))
            else
                host.log("warn", "Easee: phaseMode write failed; skipping current write to avoid overcurrent on stale phase: " ..
                    redact_http_err(pm_err))
                -- CRITICAL: power_w was budgeted against requested_phases
                -- (e.g. 7400 W with the intent of 3Φ → 32 A/phase).
                -- Computing amps against the unchanged phase count would
                -- command 32 A on a single phase — tripping a 16 A
                -- breaker. Fail the command; controller retries next tick.
                return false
            end
        end

        local amps = per_phase_amps(power_w, voltage, phases, max_a)
        local err = write_setting(charger_serial, {dynamicChargerCurrent = amps})
        if err == nil then
            last_amps_set = amps
            -- After a phaseMode change, schedule one re-write of
            -- dynamicChargerCurrent on the next driver_poll. Easee's
            -- cloud resets it to maxChargerCurrent during the phase
            -- transition; this defends against the reset window
            -- without waiting for the controller's 5 s tick.
            if phase_changed then
                pending_amp_resend = true
                pending_amp_resend_at_ms = now_ms + 5000
            end
            -- Auto-resume after pause. The controller's contract used
            -- to be: ev_pause → (later) ev_resume → ev_set_current.
            -- In practice the controller drops ev_resume in some
            -- transitions and just re-issues ev_set_current with a
            -- non-zero offer — Easee accepts the new
            -- dynamicChargerCurrent but the contactor stays open
            -- because pause is still active (op_mode=2,
            -- reason_no_current=52 or 100, ev_w=0). Defensively
            -- send resume_charging when we know we're paused and
            -- the new offer is > 0. Idempotent on the Easee side;
            -- a failure here doesn't fail the command (the offer
            -- itself succeeded, controller will retry next tick).
            if amps > 0 and paused_state then
                if post_command("/commands/resume_charging") then
                    paused_state = false
                    host.log("info", "Easee: auto-resumed after pause (offer=" ..
                        tostring(amps) .. " A)")
                end
            end
        end
        return err == nil
    end

    return false
end

function driver_default_mode()
    -- No-op — cloud charger manages itself.
end

function driver_cleanup()
    access_token = nil
    refresh_token = nil
end
