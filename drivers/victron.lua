-- victron.lua
-- Victron Energy Venus OS / Cerbo GX Modbus TCP driver
-- Emits: PV, Battery, Meter telemetry (READ-ONLY)

DRIVER = {
  id           = "victron",
  name         = "Victron Energy GX",
  manufacturer = "Victron Energy",
  version      = "1.0.0",
  protocols    = { "modbus" },
  capabilities = { "meter", "pv", "battery" },
  description  = "Victron Cerbo GX / Venus GX monitoring via Modbus TCP.",
  homepage     = "https://www.victronenergy.com",
  authors      = { "FTW contributors" },
  tested_models = { "Cerbo GX", "Venus GX" },
  verification_status = "experimental",
  verification_notes = "Ported from a reference implementation. Not yet verified against live hardware on a FTW site.",
  connection_defaults = {
    port    = 502,
    unit_id = 1,
  },
}
--
-- Register conventions:
--   Register type: HOLDING (FC 0x03)
--   Reads from Venus OS / Cerbo GX acting as a Modbus server.
--   Unit ID 100 is the "system" aggregate — pre-summed across all inverters,
--   chargers, and MPPTs wired to the GX. Configure unit_id: 100 in YAML.
--
-- Sign convention (site, after this driver):
--   pv.w      negative = generation (this driver negates at the boundary)
--   battery.w positive = charging, negative = discharging
--   meter.w   positive = import, negative = export
--
-- Victron reports grid power already with import positive / export negative,
-- so meter.w passes through unchanged. PV is reported positive and is negated
-- here. Battery power at register 842 is reported with the same sign as the
-- site convention (positive = charging), so it passes through unchanged.
-- (The legacy hugin driver negated it; that driver was tagged community/
-- untested and the inversion looks like a misread of the register map.)

PROTOCOL = "modbus"

----------------------------------------------------------------------------
-- Initialization
----------------------------------------------------------------------------

function driver_init(config)
    host.set_make("Victron")

    -- Serial number: unit 100, register 800 (product id) + 801 is reserved.
    -- The Venus OS system service doesn't expose a scalar SN at unit 100, so
    -- we leave set_sn for a future per-inverter driver variant. Device
    -- identity falls back to ARP/endpoint resolution — see
    -- docs/device-identity.md.
end

----------------------------------------------------------------------------
-- Telemetry polling
----------------------------------------------------------------------------

function driver_poll()
    ------------------------------------------------------------------
    -- PV (solar generation)
    ------------------------------------------------------------------

    -- PV AC-coupled output power: registers 808/809/810 (U16, W per phase L1/L2/L3).
    -- Sum all three phases for total AC-coupled PV.
    local ok_pvac, pvac_regs = pcall(host.modbus_read, 808, 3, "holding")
    local pv_ac_w = 0
    if ok_pvac and pvac_regs then
        pv_ac_w = (pvac_regs[1] or 0) + (pvac_regs[2] or 0) + (pvac_regs[3] or 0)
    end

    -- PV DC-coupled MPPT output power: register 850 (U16, W)
    local ok_pvdc, pvdc_regs = pcall(host.modbus_read, 850, 1, "holding")
    local pv_dc_w = 0
    if ok_pvdc and pvdc_regs then
        pv_dc_w = pvdc_regs[1]
    end

    local pv_total = pv_ac_w + pv_dc_w

    -- Site convention: PV generation is negative (power flowing out of the
    -- array into the site).
    host.emit("pv", {
        w = -pv_total,
    })

    ------------------------------------------------------------------
    -- Meter (grid connection point)
    ------------------------------------------------------------------

    -- Grid per-phase power: 820/821/822 (I16, W), import positive (matches site).
    -- NOTE: 823-825 are genset power (not voltage), 826 is active-input source
    -- (not current). Only 820-822 are grid power registers on Venus OS unit 100.
    local ok_grid, grid_regs = pcall(host.modbus_read, 820, 3, "holding")
    local l1_w, l2_w, l3_w = 0, 0, 0
    if ok_grid and grid_regs then
        l1_w = host.decode_i16(grid_regs[1])
        l2_w = host.decode_i16(grid_regs[2])
        l3_w = host.decode_i16(grid_regs[3])
    end

    local grid_total_w = l1_w + l2_w + l3_w

    host.emit("meter", {
        w    = grid_total_w,
        l1_w = l1_w,
        l2_w = l2_w,
        l3_w = l3_w,
    })

    -- Diagnostics: long-format TS DB
    host.emit_metric("meter_l1_w", l1_w)
    host.emit_metric("meter_l2_w", l2_w)
    host.emit_metric("meter_l3_w", l3_w)

    ------------------------------------------------------------------
    -- Battery
    ------------------------------------------------------------------

    -- Battery block: 840-844 in one atomic read.
    --   840 — voltage  (U16, 0.1 V)
    --   841 — current  (I16, 0.1 A) — positive = charging
    --   842 — power    (I16, W)     — positive = charging (matches site)
    --   843 — SoC      (U16, %)     — convert to 0.0–1.0 fraction
    --   844 — temp     (I16, 0.1 C)
    local ok_bat, bat_regs = pcall(host.modbus_read, 840, 5, "holding")
    local bat_v, bat_a, bat_w, bat_soc, bat_temp = 0, 0, 0, 0, 0
    if ok_bat and bat_regs then
        bat_v    = bat_regs[1] * 0.1
        bat_a    = host.decode_i16(bat_regs[2]) * 0.1
        bat_w    = host.decode_i16(bat_regs[3])
        bat_soc  = bat_regs[4] / 100
        bat_temp = host.decode_i16(bat_regs[5]) * 0.1
    end

    host.emit("battery", {
        w      = bat_w,
        v      = bat_v,
        a      = bat_a,
        soc    = bat_soc,
        temp_c = bat_temp,
    })
    host.emit_metric("battery_dc_v",    bat_v)
    host.emit_metric("battery_dc_a",    bat_a)
    host.emit_metric("battery_temp_c",  bat_temp)

    return 5000
end

----------------------------------------------------------------------------
-- Control (read-only driver)
----------------------------------------------------------------------------

-- This driver is READ-ONLY: no battery force-charge, no export curtailment.
-- Victron's ESS module accepts control via /Hub4/DisableCharge, /DisableFeedIn
-- and /AcPowerSetpoint on service com.victronenergy.settings, but those are
-- dbus-first and are not in the default Modbus register map. Extend this
-- driver if/when we validate the write path on a real Cerbo GX.
function driver_command(action, power_w, cmd)
    if action == "init" or action == "deinit" then
        return true
    end
    return false
end

function driver_default_mode()
    -- Nothing to revert — device runs in its own ESS mode autonomously.
end

function driver_cleanup()
end
