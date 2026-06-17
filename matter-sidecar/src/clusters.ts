// Maps numeric Matter cluster/attribute/command IDs (the only vocabulary
// drivers/matter.lua and its YAML config know) onto matter.js's named
// cluster definitions, which is what the Read/Write/Invoke request builders
// in @matter/protocol actually need to encode values correctly on the wire.
import * as Clusters from "@matter/types/clusters";

export interface ClusterModule {
  id: number;
  name: string;
  attributes: Record<string, { id: number; name: string }>;
  commands: Record<string, { id: number; name: string }>;
  Cluster: unknown;
}

const byId = new Map<number, ClusterModule>();

for (const key of Object.keys(Clusters)) {
  const value = (Clusters as Record<string, unknown>)[key];
  if (
    value &&
    typeof value === "object" &&
    !key.endsWith("Cluster") &&
    typeof (value as ClusterModule).id === "number" &&
    (value as ClusterModule).attributes &&
    (value as ClusterModule).Cluster
  ) {
    const mod = value as ClusterModule;
    if (!byId.has(mod.id)) byId.set(mod.id, mod);
  }
}

export function clusterFor(clusterId: number): ClusterModule {
  const mod = byId.get(clusterId);
  if (!mod) throw new Error(`unknown cluster 0x${clusterId.toString(16)}`);
  return mod;
}

export function attributeNameFor(cluster: ClusterModule, attributeId: number): string {
  for (const [name, def] of Object.entries(cluster.attributes)) {
    if (def.id === attributeId) return name;
  }
  throw new Error(`unknown attribute ${attributeId} on cluster ${cluster.name}`);
}
