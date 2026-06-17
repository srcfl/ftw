// Phase 3 — 42W as a Matter *bridge*: surfaces non-Matter DERs (inverters,
// batteries, EVSE chargers, ...) as bridged Matter devices so other Matter
// ecosystems (Apple Home, Home Assistant, ...) can see their live power
// draw. Each 42W driver becomes one Aggregator child: a BridgedNode
// endpoint (identity + reachability) wrapping one ElectricalMeterDevice
// child endpoint (the actual power/energy clusters). The Go side decides
// which drivers are bridged and owns all unit conversion; this module only
// creates/updates endpoints.
//
// SoC exposure (PowerSourceServer's Battery feature) is left for a
// follow-up — it pulls in its own feature-conformance requirements beyond
// this MVP's scope.
import { Endpoint } from "@matter/node";
import { AggregatorEndpoint, BridgedNodeEndpoint } from "@matter/node/endpoints";
import { ElectricalMeterDevice, ElectricalMeterRequirements } from "@matter/node/devices";

// The bare ElectricalMeterDevice export has no bound server behaviors
// (its `behaviors` type is `{}`) — .with(...) must be called with the
// requirement's own default server classes to get a type that actually
// exposes electricalPowerMeasurement/electricalEnergyMeasurement.
const ElectricalMeterDeviceWithServers = ElectricalMeterDevice.with(
  ElectricalMeterRequirements.ElectricalPowerMeasurementServer,
  ElectricalMeterRequirements.ElectricalEnergyMeasurementServer,
);

export interface BridgedDeviceInput {
  id: string;
  name: string;
  device_type: string;
  power_mw: number;
}

interface BridgeEntry {
  node: Endpoint<typeof BridgedNodeEndpoint>;
  meter: Endpoint<typeof ElectricalMeterDeviceWithServers>;
}

function sanitizeId(id: string): string {
  return id.replace(/[^a-zA-Z0-9_-]/g, "_");
}

export class Bridge {
  readonly endpoint: Endpoint<typeof AggregatorEndpoint>;
  private readonly entries = new Map<string, BridgeEntry>();

  constructor() {
    this.endpoint = new Endpoint(AggregatorEndpoint, { id: "bridge" });
  }

  async sync(devices: BridgedDeviceInput[]): Promise<void> {
    const seen = new Set<string>();
    for (const dev of devices) {
      seen.add(dev.id);
      let entry = this.entries.get(dev.id);
      if (!entry) {
        entry = this.createEntry(dev);
        this.entries.set(dev.id, entry);
      }
      await entry.node.set({
        bridgedDeviceBasicInformation: { nodeLabel: dev.name, reachable: true },
      });
      await entry.meter.set({
        electricalPowerMeasurement: { activePower: Math.round(dev.power_mw) },
      });
    }
    for (const [id, entry] of this.entries) {
      if (!seen.has(id)) {
        await entry.node.set({ bridgedDeviceBasicInformation: { reachable: false } });
      }
    }
  }

  private createEntry(dev: BridgedDeviceInput): BridgeEntry {
    const safeId = sanitizeId(dev.id);
    const meter = new Endpoint(ElectricalMeterDeviceWithServers, {
      id: `${safeId}-meter`,
      electricalPowerMeasurement: {
        powerMode: 0, // Unknown — 42W abstracts AC/DC away above the driver layer
        numberOfMeasurementTypes: 1,
        accuracy: [],
        activePower: null,
      },
      electricalEnergyMeasurement: {
        accuracy: {
          measurementType: 0,
          measured: true,
          minMeasuredValue: -1e15,
          maxMeasuredValue: 1e15,
          accuracyRanges: [],
        },
      },
    });
    const node = new Endpoint(BridgedNodeEndpoint, {
      id: safeId,
      bridgedDeviceBasicInformation: {
        nodeLabel: dev.name,
        reachable: true,
        uniqueId: dev.id,
      },
      parts: [meter],
    });
    this.endpoint.parts.add(node);
    return { node, meter };
  }
}
