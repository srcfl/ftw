-- matter.lua
-- Generic Matter device driver via the 42W Matter controller sidecar.
-- Works with any Matter-certified device: thermostats, smart plugs,
-- energy meters, on/off switches, etc. All behaviour is config-driven.
--
-- The driver reads a list of cluster attributes each poll and emits them
-- as scalar metrics (host.emit_metric) or, optionally, as structured
-- telemetry (host.emit "meter"|"pv"|"battery") for DER-type devices.
-- Commands from the dispatch layer are mapped to attribute writes or
-- cluster command invocations via the `commands` config block.
--
-- Device onboarding (multi-fabric — 42W does NOT commission):
--   1. Commission the device with whatever controller it shipped with
--      (Apple Home, Google Home, Home Assistant, SmartThings, ...).
--   2. In that controller, use "share device" / "add to another network"
--      / "pair to another Matter controller" to mint a one-time pairing
--      code (a Manual Pairing Code or QR setup code).
--   3. Hand that code to 42W's Matter sidecar (matter-sidecar/, built on
--      matter.js) by sending it a `commission` request over its WebSocket
--      API with {"pairing_code": "<code>"} — there is no UI for this yet;
--      use e.g. `websocat ws://localhost:5580/ws` or a small script. The
--      sidecar joins the device as an additional fabric admin over plain
--      IP (no BLE on our side, so the whole Thread-vs-Wi-Fi transport
--      question is moot for us) and returns a small integer node_id.
--   4. Put that node_id in this driver's config below.
--
-- Prerequisites: the Matter sidecar (see docker-compose.yml's
-- matter-sidecar service) must be running and reachable.
--
-- Config example — Wiser thermostat (Schneider Electric):
--
--   drivers:
--     - name: living_room
--       lua: drivers/matter.lua
--       capabilities:
--         matter:
--           host: localhost   # Matter sidecar address
--       config:
--         node_id: 1234       # assigned by the sidecar after the share-code join
--         make: "Schneider Electric"
--         model: "Wiser"
--         poll_interval_ms: 30000
--         reads:
--           - name: indoor_temp_c
--             endpoint: 1
--             cluster: 0x0201   # Thermostat
--             attribute: 0x0000 # LocalTemperature
--             scale: 0.01       # raw value is °C × 100
--           - name: heating_setpoint_c
--             endpoint: 1
--             cluster: 0x0201
--             attribute: 0x0012 # OccupiedHeatingSetpoint
--             scale: 0.01
--         commands:
--           setpoint:           # driver_command action name
--             endpoint: 1
--             cluster: 0x0201
--             attribute: 0x0012
--             scale: 100        # cmd.value (°C) × 100 before writing
--
-- Config example — smart plug with power metering:
--
--   config:
--     node_id: 5678
--     reads:
--       - name: power_w
--         endpoint: 1
--         cluster: 0x0B04   # Electrical Measurement
--         attribute: 0x050B # ActivePower
--         emit_as: meter    # also calls host.emit("meter", {w = value})
--     commands:
--       on_off:
--         endpoint: 1
--         cluster: 0x0006   # On/Off
--         invoke: "Toggle"  # cluster command name (instead of attribute write)
--
-- Config example — standalone temperature sensor (Matter Temperature
-- Measurement cluster). Pair a cheap Matter room sensor and feed its reading
-- to a thermostat zone via flexloads `indoor_driver` so the thermal model
-- uses true room temperature, not the thermostat's mounting-biased probe:
--
--   drivers:
--     - name: bedroom_temp
--       lua: drivers/matter.lua
--       capabilities:
--         matter: { host: localhost }
--       config:
--         node_id: 9012
--         reads:
--           - name: indoor_temp_c
--             endpoint: 1
--             cluster: 0x0402   # Temperature Measurement
--             attribute: 0x0000 # MeasuredValue
--             scale: 0.01       # raw is °C × 100
--   flexloads:
--     - type: thermostat
--       driver_name: living_room      # the thermostat we command
--       indoor_driver: bedroom_temp   # but read temperature from the sensor
--       indoor_metric: indoor_temp_c
--       ...
--
-- Cluster IDs may be written as hex (0x0201) or decimal (513) — YAML parses
-- both as integers, so the driver receives them as numbers either way.
--
-- Protocol: Matter (via the 42W Matter controller sidecar)

DRIVER = {
  id           = "matter-generic",
  name         = "Matter (generic)",
  version      = "1.0.0",
  protocols    = { "matter" },
  description  = "Generic Matter device driver. Config-driven attribute reads and commands. Works with any Matter-certified device shared to 42W multi-fabric.",
  authors      = { "forty-two-watts contributors" },
  verification_status = "experimental",
}

local node_id = nil
local reads   = {}  -- list of {name, endpoint, cluster, attribute, scale, emit_as}
local cmds    = {}  -- map of action → {endpoint, cluster, attribute?, invoke?, scale?}
local err_count = 0
local MAX_CONSECUTIVE_ERRORS = 5

local function parse_number(v)
    -- config values arrive as Lua numbers already (YAML hex + decimal both
    -- parse to integers in Go, then arrive here as LNumber).
    return tonumber(v)
end

function driver_init(cfg)
    cfg = cfg or {}

    node_id = parse_number(cfg.node_id)
    if not node_id then
        host.log("error", "matter: node_id is required in config")
        return
    end

    if cfg.make then host.set_make(tostring(cfg.make)) end
    -- Anchor device identity on the fabric-unique node_id, not the model name.
    -- Two identical Wiser thermostats share the same make+model but have
    -- distinct node_ids, so using model would cause a device_id collision and
    -- one device would overwrite the other's learned state.
    host.set_sn(tostring(node_id))

    local interval = parse_number(cfg.poll_interval_ms or 30000)
    host.set_poll_interval(interval)

    -- Build reads list.
    reads = {}
    local raw_reads = cfg.reads or {}
    for i = 1, #raw_reads do
        local r = raw_reads[i]
        local ep  = parse_number(r.endpoint)
        local cl  = parse_number(r.cluster)
        local att = parse_number(r.attribute)
        if ep and cl and att then
            reads[#reads + 1] = {
                name      = tostring(r.name or (cl .. "/" .. att)),
                endpoint  = ep,
                cluster   = cl,
                attribute = att,
                scale     = parse_number(r.scale or 1),
                emit_as   = r.emit_as,  -- nil | "meter" | "pv" | "battery"
            }
        else
            host.log("warn", "matter: skipping read with missing endpoint/cluster/attribute")
        end
    end

    -- Build commands map.
    cmds = {}
    local raw_cmds = cfg.commands or {}
    for action, spec in pairs(raw_cmds) do
        cmds[tostring(action)] = {
            endpoint  = parse_number(spec.endpoint),
            cluster   = parse_number(spec.cluster),
            attribute = parse_number(spec.attribute),   -- for write_attribute mode
            invoke    = spec.invoke and tostring(spec.invoke) or nil, -- for invoke mode
            scale     = parse_number(spec.scale or 1),
        }
    end

    host.log("info", string.format(
        "matter: init node_id=%d  reads=%d  commands=%d",
        node_id, #reads, (function() local n=0; for _ in pairs(cmds) do n=n+1 end; return n end)()
    ))
end

function driver_poll()
    if not node_id then return end

    local had_error = false

    for _, r in ipairs(reads) do
        local val, err = host.matter_read(node_id, r.endpoint, r.cluster, r.attribute)
        if err then
            host.log("warn", string.format(
                "matter: read %s (ep=%d cl=0x%04X att=0x%04X): %s",
                r.name, r.endpoint, r.cluster, r.attribute, tostring(err)
            ))
            had_error = true
        else
            local v = tonumber(val)
            if v == nil then
                host.log("warn", "matter: " .. r.name .. ": non-numeric value: " .. tostring(val))
                had_error = true
            else
                local scaled = v * r.scale

                -- Always emit as a scalar metric for the TS DB.
                host.emit_metric(r.name, scaled)

                -- Optionally also emit structured DER telemetry so the
                -- device shows up in the dashboard / MPC as a proper DER.
                if r.emit_as == "meter" then
                    host.emit("meter", { w = scaled })
                elseif r.emit_as == "pv" then
                    host.emit("pv", { w = scaled })
                elseif r.emit_as == "battery" then
                    host.emit("battery", { w = scaled })
                end
            end
        end
    end

    if had_error then
        err_count = err_count + 1
        if err_count >= MAX_CONSECUTIVE_ERRORS then
            host.log("error", string.format(
                "matter: %d consecutive poll errors for node %d",
                err_count, node_id
            ))
        end
    else
        err_count = 0
    end
end

-- driver_command is called by the dispatch layer with:
--   action  — string from cmd.action
--   power_w — number from cmd.power_w (battery dispatch; unused here)
--   tbl     — full decoded command table
--
-- Supported actions:
--   <named>         — matches a key in config.commands; writes attribute or
--                     invokes cluster command. Value comes from tbl.value.
--   write_attribute — raw attribute write: tbl.{endpoint, cluster, attribute, value, scale?}
--   invoke          — raw cluster command:  tbl.{endpoint, cluster, command, payload?}
function driver_command(action, _power_w, tbl)
    if not node_id then return end
    tbl = tbl or {}

    -- Named command defined in config.
    local spec = cmds[action]
    if spec then
        if spec.invoke then
            -- Invoke a cluster command (e.g. On/Off On/Off, Thermostat
            -- SetpointRaiseLower). No value required — these are stateless
            -- commands. tbl.payload (optional) carries cluster command args.
            local payload = host.json_encode(tbl.payload or {})
            local _, err = host.matter_invoke(node_id, spec.endpoint, spec.cluster, spec.invoke, payload)
            if err then
                host.log("warn", "matter: invoke '" .. spec.invoke .. "': " .. tostring(err))
            end
        else
            -- Write an attribute (e.g. OccupiedHeatingSetpoint) — needs a value.
            local value = tonumber(tbl.value)
            if value == nil then
                host.log("warn", "matter: command '" .. action .. "': missing tbl.value for attribute write")
                return
            end
            local raw = math.floor(value * spec.scale + 0.5)
            local err = host.matter_write(node_id, spec.endpoint, spec.cluster, spec.attribute, raw)
            if err then
                host.log("warn", string.format(
                    "matter: write_attribute for '%s': %s", action, tostring(err)
                ))
            end
        end
        return
    end

    -- Raw write_attribute (for programmatic use by e.g. the MPC).
    if action == "write_attribute" then
        local ep  = tonumber(tbl.endpoint)
        local cl  = tonumber(tbl.cluster)
        local att = tonumber(tbl.attribute)
        local val = tonumber(tbl.value)
        local sc  = tonumber(tbl.scale or 1)
        if not (ep and cl and att and val) then
            host.log("warn", "matter: write_attribute: missing endpoint/cluster/attribute/value")
            return
        end
        local err = host.matter_write(node_id, ep, cl, att, math.floor(val * sc + 0.5))
        if err then
            host.log("warn", "matter: write_attribute: " .. tostring(err))
        end
        return
    end

    -- Raw invoke (for programmatic use).
    if action == "invoke" then
        local ep  = tonumber(tbl.endpoint)
        local cl  = tonumber(tbl.cluster)
        local cmd = tbl.command and tostring(tbl.command)
        if not (ep and cl and cmd) then
            host.log("warn", "matter: invoke: missing endpoint/cluster/command")
            return
        end
        local payload = host.json_encode(tbl.payload or {})
        local _, err = host.matter_invoke(node_id, ep, cl, cmd, payload)
        if err then
            host.log("warn", "matter: invoke '" .. cmd .. "': " .. tostring(err))
        end
        return
    end

    host.log("warn", "matter: unknown action '" .. tostring(action) .. "'")
end

function driver_default_mode()
    -- No autonomous fallback for generic Matter devices.
    -- Thermostats revert to their own internal schedule when 42W stops
    -- sending commands — that's the correct safe state.
end

function driver_cleanup()
end
