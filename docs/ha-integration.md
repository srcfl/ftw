# Home Assistant Integration

forty-two-watts integrates with Home Assistant via MQTT. It publishes sensor data, supports MQTT auto-discovery, and subscribes to command topics for mode and target changes.

## Prerequisites

1. **Mosquitto MQTT broker** running on your Home Assistant instance (install via Settings > Add-ons > Mosquitto broker)
2. An MQTT user configured for forty-two-watts (Settings > People > Users, or in the Mosquitto add-on config)
3. **MQTT integration** enabled in Home Assistant (Settings > Devices & Services > MQTT)

## Configuration

Add the `homeassistant` section to your `config.yaml`:

```yaml
homeassistant:
  enabled: true
  broker: 192.168.1.1          # IP of your HA instance / Mosquitto broker
  port: 1883                   # MQTT port (default: 1883)
  username: fortytwo            # MQTT user
  password: fortytwo            # MQTT password
  publish_interval_s: 5        # How often to publish sensor updates (default: 5)
```

All fields except `broker` have defaults. The minimal config is:

```yaml
homeassistant:
  enabled: true
  broker: 192.168.1.1
```

## Auto-Discovered Entities

forty-two-watts publishes MQTT auto-discovery configs under the `homeassistant/` prefix. Once connected, these entities appear automatically in Home Assistant.

### Site-Level Sensors

| Entity                    | Type   | Unit | Device Class | Description                          |
|---------------------------|--------|------|--------------|--------------------------------------|
| Home EMS grid power       | sensor | W    | power        | Total grid power (+import / -export) |
| Home EMS pv power         | sensor | W    | power        | Total PV generation                  |
| Home EMS battery power    | sensor | W    | power        | Total battery power (+charge / -discharge) |
| Home EMS battery soc      | sensor | %    | battery      | Weighted average battery SoC         |

### Mode Control

| Entity          | Type   | Options                                              | Description         |
|-----------------|--------|------------------------------------------------------|---------------------|
| Home EMS Mode   | select | `idle`, `self_consumption`, `peak_shaving`, `charge`, `priority`, `weighted`, `planner_self`, `planner_cheap`, `planner_passive_arbitrage`, `planner_arbitrage` | Operating mode      |

### Per-Driver Sensors

For each configured driver (e.g., `ferroamp`, `sungrow`), the following sensors are created:

| Entity                         | Unit | Device Class | Description                |
|--------------------------------|------|--------------|----------------------------|
| Home EMS {driver} meter w      | W    | power        | Grid power from this driver|
| Home EMS {driver} pv w         | W    | power        | PV power from this driver  |
| Home EMS {driver} bat w        | W    | power        | Battery power              |
| Home EMS {driver} bat soc      | %    | battery      | Battery state of charge    |

## MQTT Topics

### Published Topics (State)

forty-two-watts publishes to these topics at the configured interval:

| Topic                              | Payload   | Description                          |
|------------------------------------|-----------|--------------------------------------|
| `fortytwo/status/grid_w`            | number    | Total grid power in watts            |
| `fortytwo/status/pv_w`             | number    | Total PV power in watts              |
| `fortytwo/status/bat_w`            | number    | Total battery power in watts         |
| `fortytwo/status/bat_soc`          | number    | Weighted avg battery SoC (0-100%)    |
| `fortytwo/status/mode`             | string    | Current operating mode               |
| `fortytwo/drivers/{name}/meter_w`  | number    | Per-driver grid power                |
| `fortytwo/drivers/{name}/pv_w`     | number    | Per-driver PV power                  |
| `fortytwo/drivers/{name}/bat_w`    | number    | Per-driver battery power             |
| `fortytwo/drivers/{name}/bat_soc`  | number    | Per-driver battery SoC (0-100%)      |
| `fortytwo/drivers/{name}/status`   | string    | Driver status: `Ok`, `Degraded`, `Offline` |

### Command Topics (Control)

Home Assistant (or any MQTT client) can publish to these topics to control forty-two-watts:

| Topic                          | Payload        | Description                            |
|--------------------------------|----------------|----------------------------------------|
| `fortytwo/command/mode`         | string         | Set mode: `idle`, `self_consumption`, `peak_shaving`, `charge`, `priority`, `weighted`, `planner_self`, `planner_cheap`, `planner_passive_arbitrage`, `planner_arbitrage` |
| `fortytwo/command/grid_target_w`| number (string)| Set grid target in watts (e.g., `"0"` for self-consumption, `"200"` for 200W import target) |

## Example Home Assistant Automations

### Price-Based Charging

Charge batteries during cheap electricity hours, switch to self-consumption otherwise.

```yaml
automation:
  - alias: "Charge batteries during cheap hours"
    trigger:
      - platform: time
        at: "02:00:00"
    action:
      - service: mqtt.publish
        data:
          topic: "fortytwo/command/mode"
          payload: "charge"

  - alias: "Self-consumption during day"
    trigger:
      - platform: time
        at: "06:00:00"
    action:
      - service: mqtt.publish
        data:
          topic: "fortytwo/command/mode"
          payload: "self_consumption"
```

### Price-Based with Nordpool Integration

Use the Nordpool integration for dynamic electricity pricing:

```yaml
automation:
  - alias: "Charge when electricity is cheap"
    trigger:
      - platform: numeric_state
        entity_id: sensor.nordpool_kwh_se3_sek_3_10_025
        below: 0.30
    action:
      - service: mqtt.publish
        data:
          topic: "fortytwo/command/mode"
          payload: "charge"

  - alias: "Self-consumption when electricity is expensive"
    trigger:
      - platform: numeric_state
        entity_id: sensor.nordpool_kwh_se3_sek_3_10_025
        above: 0.80
    action:
      - service: mqtt.publish
        data:
          topic: "fortytwo/command/mode"
          payload: "self_consumption"
```

### Weather-Based Mode Switching

Switch to idle mode on sunny days (PV covers load), charge on cloudy days:

```yaml
automation:
  - alias: "Idle mode on sunny days"
    trigger:
      - platform: numeric_state
        entity_id: sensor.forty_two_watts_pv_power
        below: -3000   # More than 3kW PV generation (negative = generating)
        for: "00:15:00"
    action:
      - service: mqtt.publish
        data:
          topic: "fortytwo/command/mode"
          payload: "self_consumption"
```

### Low Battery Protection

Switch to idle when battery is very low to prevent deep discharge:

```yaml
automation:
  - alias: "Idle when battery critically low"
    trigger:
      - platform: numeric_state
        entity_id: sensor.forty_two_watts_battery_soc
        below: 10
    action:
      - service: mqtt.publish
        data:
          topic: "fortytwo/command/mode"
          payload: "idle"

  - alias: "Resume self-consumption when battery recovers"
    trigger:
      - platform: numeric_state
        entity_id: sensor.forty_two_watts_battery_soc
        above: 20
    action:
      - service: mqtt.publish
        data:
          topic: "fortytwo/command/mode"
          payload: "self_consumption"
```

### Grid Export Limit

Set a grid target to export a maximum of 5 kW (useful for feed-in limits):

```yaml
automation:
  - alias: "Set feed-in limit"
    trigger:
      - platform: homeassistant
        event: start
    action:
      - service: mqtt.publish
        data:
          topic: "fortytwo/command/grid_target_w"
          payload: "-5000"
```

## Dashboard Card Examples

### Energy Distribution Card

A simple Lovelace card showing the energy flow:

```yaml
type: entities
title: Home EMS
entities:
  - entity: sensor.forty_two_watts_grid_power
    name: Grid
    icon: mdi:transmission-tower
  - entity: sensor.forty_two_watts_pv_power
    name: Solar
    icon: mdi:solar-power
  - entity: sensor.forty_two_watts_battery_power
    name: Battery
    icon: mdi:battery-charging
  - entity: sensor.forty_two_watts_battery_soc
    name: Battery SoC
    icon: mdi:battery
  - entity: select.forty_two_watts_mode
    name: Mode
```

### Gauge Cards for Power Flow

```yaml
type: horizontal-stack
cards:
  - type: gauge
    entity: sensor.forty_two_watts_grid_power
    name: Grid
    min: -10000
    max: 10000
    severity:
      green: -10000
      yellow: 0
      red: 5000
  - type: gauge
    entity: sensor.forty_two_watts_battery_soc
    name: Battery
    min: 0
    max: 100
    severity:
      red: 0
      yellow: 20
      green: 50
```

### Per-Driver Status

```yaml
type: entities
title: Driver Status
entities:
  - entity: sensor.forty_two_watts_ferroamp_meter_w
    name: Ferroamp Grid
  - entity: sensor.forty_two_watts_ferroamp_bat_w
    name: Ferroamp Battery
  - entity: sensor.forty_two_watts_ferroamp_bat_soc
    name: Ferroamp SoC
  - entity: sensor.forty_two_watts_sungrow_meter_w
    name: Sungrow Grid
  - entity: sensor.forty_two_watts_sungrow_bat_w
    name: Sungrow Battery
  - entity: sensor.forty_two_watts_sungrow_bat_soc
    name: Sungrow SoC
```

### History Graph

```yaml
type: history-graph
title: Energy (Last 24h)
hours_to_show: 24
entities:
  - entity: sensor.forty_two_watts_grid_power
    name: Grid
  - entity: sensor.forty_two_watts_pv_power
    name: Solar
  - entity: sensor.forty_two_watts_battery_power
    name: Battery
```

## Troubleshooting

**Entities not appearing in HA:**
- Verify the Mosquitto add-on is running and MQTT integration is configured
- Check that forty-two-watts can reach the broker: `mosquitto_pub -h <broker-ip> -u fortytwo -P fortytwo -t test -m hello`
- Check forty-two-watts logs for MQTT connection errors

**Stale sensor values:**
- Verify `publish_interval_s` is set appropriately (default 5 seconds)
- Check driver health in the API: `curl http://<ems-ip>:8080/api/drivers`

**Mode select not working:**
- Ensure the MQTT integration in HA has `command_topic` support
- Check that the payload matches exactly: `self_consumption` (not `self-consumption` or `Self Consumption`)
