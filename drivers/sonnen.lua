-- sonnenBatterie — local JSON API v2 (read-only)
-- Emits: battery
-- Protocol: HTTP (local JSON API v2 on the Sonnen unit). Read API must
-- be enabled in the Sonnen web UI under Software-Integration → JSON API,
-- which also surfaces the Auth-Token to put into config.api_token.
--
-- Config example (config.yaml):
--   drivers:
--     - name: sonnen
--       lua: drivers/sonnen.lua
--       capabilities:
--         http:
--           allowed_hosts:
--             - 192.168.x.y     # LAN IP of the Sonnen unit
--       config:
--         host: "192.168.x.y"
--         port: 80
--         api_token: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
--
-- Sign convention (site: positive W flows INTO the site):
--   battery.w: positive = charging (sink), negative = discharging (source).
--   Sonnen Pac_total_W is reported positive=charging, so it passes through
--   unchanged. This is from community-contributed sample code; verify
--   on first install by checking that battery.w is positive while the
--   site is exporting and the Sonnen is taking the surplus.

DRIVER = {
  host_api_min = 1,
  host_api_max = 1,
  id           = "sonnen",
  name         = "sonnenBatterie (local API)",
  manufacturer = "sonnen",
  version      = "1.0.0",
  protocols    = { "http" },
  capabilities = { "battery" },
  description  = "sonnenBatterie local JSON API v2: SoC + charge/discharge power. Read-only.",
  homepage     = "https://sonnen.de",
  authors      = { "FTW contributors" },
  http_hosts   = { },
  verification_status = "experimental",
  verification_notes = "Community-contributed local-API driver; not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    host = "",
    port = 80,
  },
  -- Secret config keys the wizard / Settings UI should render password
  -- inputs for and stuff into config.<key>. Keeps Auth-Token out of
  -- yaml-by-hand and lets the operator paste it from the Sonnen web UI
  -- (Software-Integration → JSON API).
  config_secrets = { "api_token" },
}

PROTOCOL = "http"

local sonnen_host = ""
local sonnen_port = 80
local api_token   = ""

-- Backoff state. Doubles from 2 s to 60 s on consecutive HTTP / decode
-- failures so a powered-down or wrong-token unit doesn't pin the poll
-- loop hammering the LAN.
local last_attempt = 0
local backoff_ms   = 0
local BACKOFF_MIN  =  2000
local BACKOFF_MAX  = 60000

local function base_url()
    return "http://" .. sonnen_host .. ":" .. tostring(sonnen_port)
end

local function bump_backoff()
    if backoff_ms == 0 then
        backoff_ms = BACKOFF_MIN
    else
        backoff_ms = math.min(backoff_ms * 2, BACKOFF_MAX)
    end
    last_attempt = host.millis()
end

local function clear_backoff()
    backoff_ms   = 0
    last_attempt = 0
end

local function in_backoff()
    if backoff_ms == 0 then return false end
    return (host.millis() - last_attempt) < backoff_ms
end

-- clamp drops obviously-broken values (NaN, sentinel huge numbers from a
-- partial JSON parse) so a single bad poll doesn't poison the battery
-- model with a 2 GW reading.
local function clamp(v, max_abs)
    local n = tonumber(v)
    if n == nil then return nil end
    if max_abs and math.abs(n) > max_abs then return nil end
    return n
end

----------------------------------------------------------------------------
-- Driver interface
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("sonnen")

    if config then
        if type(config.host) == "string" and config.host ~= "" then
            sonnen_host = config.host
        end
        if config.port then
            sonnen_port = tonumber(config.port) or 80
        end
        if type(config.api_token) == "string" and config.api_token ~= "" then
            api_token = config.api_token
        end
    end

    if sonnen_host == "" then
        host.log("error", "sonnen: config.host required (LAN IP of the Sonnen unit)")
        return
    end
    if api_token == "" then
        host.log("warn", "sonnen: config.api_token is empty — JSON API v2 requires an Auth-Token; enable it in the Sonnen web UI under Software-Integration")
    end

    host.log("info", "sonnen: driver initialized (host=" .. sonnen_host
        .. ":" .. tostring(sonnen_port) .. ")")
end

function driver_poll()
    if sonnen_host == "" then
        return 10000
    end
    if in_backoff() then
        return 1000
    end

    local url = base_url() .. "/api/v2/latestdata"
    local headers = { ["Auth-Token"] = api_token }
    local body, err = host.http_get(url, headers)

    if err then
        bump_backoff()
        host.log("warn", "sonnen: fetch failed: " .. tostring(err)
            .. " (retry in " .. backoff_ms .. "ms)")
        return 1000
    end

    local data = host.json_decode(body)
    if not data then
        bump_backoff()
        host.log("warn", "sonnen: JSON decode failed")
        return 1000
    end

    clear_backoff()

    local pac_w   = clamp(data.Pac_total_W,   30000) or 0
    local rsoc    = clamp(data.RSOC,            100)
    -- USOC is the user-visible SoC the unit clamps usable energy against;
    -- prefer it over RSOC (raw SoC) so the EMS sees the same headroom the
    -- Sonnen will actually deliver / accept.
    local usoc    = clamp(data.USOC,            100)
    local prod_w  = clamp(data.Production_W,  30000) or 0
    local cons_w  = clamp(data.Consumption_W, 30000) or 0
    local cap_wh  = clamp(data.FullChargeCapacity, 1000000)

    local soc_pct = usoc or rsoc or 0
    local battery = {
        w   = pac_w,
        soc = soc_pct / 100.0,
    }
    if cap_wh then
        battery.capacity_wh = cap_wh
    end

    host.emit("battery", battery)

    host.emit_metric("battery_rsoc",          rsoc or 0)
    host.emit_metric("battery_usoc",          usoc or 0)
    host.emit_metric("battery_pac_w",         pac_w)
    host.emit_metric("battery_production_w",  prod_w)
    host.emit_metric("battery_consumption_w", cons_w)
    if cap_wh then
        host.emit_metric("battery_capacity_wh", cap_wh)
    end

    return 1000
end

function driver_cleanup()
    clear_backoff()
end
