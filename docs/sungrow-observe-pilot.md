# Sungrow observe-only pilot

Do not activate Sungrow 1.3.1 on a live site. It can write even when its target
has `control_enabled=false`. The first site package is 1.3.2 or later and must
stay read-only: `read_only=true`, only `modbus.read`, no commands and no write
calls.

The public target currently blocks every model and firmware pair. Before a
site test, record and review:

- exact inverter model and family: SG-CX, SG-RT, SH-RS, SH-RT or SH-T;
- firmware revision and the matching public register-map revision;
- LAN connection type, port, and Modbus device ID;
- whether a battery and external site meter are present.

Do not put an IP address, serial number, credential or site ID in GitHub issues
or fleet inventory.

Test the local unsigned target first with **Test connection** or
`POST /api/drivers/test`. A Lua file change needs a new test. An active driver
needs a restart or a real config change that restarts the registry entry.

Compare these read-only values with the inverter UI or app and the site meter:

- PV power and MPPT power;
- battery power sign and size, battery voltage, current and SoC;
- grid import/export sign and total power;
- L1, L2 and L3 power, voltage and current;
- inverter temperature, grid frequency and running state.

Stop if the model or firmware does not match an approved public profile, any
sign differs, values are stale, or FTW attempts a write. After a local pass,
the release proof must bind the exact public commit and tree to the private
lock, artifact hash, signed beta index, FTW install and activation, rollback,
and Nova inventory. Keep the bundled Sungrow driver as recovery until the
signed target has parity and physical HIL evidence.

Control stays off until FTW has per-driver process or heap isolation, host
write denial, leases, safe default mode, command results and readback, and the
exact Sungrow model has passed physical HIL.
