// drivers/matter.lua config uses small integer node_ids, but Matter's real
// NodeId is a 64-bit value assigned during commissioning. This module keeps
// a small persisted mapping from "logical" node_id (handed to the operator
// after a pairing-code join, then pasted into driver config) to the real
// Matter NodeId string, so the rest of the sidecar can stay in the vocabulary
// the Lua driver already speaks.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "fs";
import { dirname } from "path";

export class NodeMap {
  private readonly path: string;
  private logicalToReal: Record<string, string> = {};

  constructor(path: string) {
    this.path = path;
    if (existsSync(path)) {
      this.logicalToReal = JSON.parse(readFileSync(path, "utf8"));
    }
  }

  realFor(logicalNodeId: number): string {
    const real = this.logicalToReal[String(logicalNodeId)];
    if (!real) throw new Error(`unknown node_id ${logicalNodeId}`);
    return real;
  }

  list(): { node_id: number; matter_node_id: string }[] {
    return Object.entries(this.logicalToReal).map(([logical, real]) => ({
      node_id: Number(logical),
      matter_node_id: real,
    }));
  }

  // assign hands out the next free logical id for a freshly commissioned
  // peer and persists the mapping immediately, so a sidecar crash right
  // after commissioning doesn't orphan the join.
  assign(realNodeId: string): number {
    let next = 1;
    for (const k of Object.keys(this.logicalToReal)) {
      const n = Number(k);
      if (n >= next) next = n + 1;
    }
    this.logicalToReal[String(next)] = realNodeId;
    this.save();
    return next;
  }

  private save(): void {
    mkdirSync(dirname(this.path), { recursive: true });
    writeFileSync(this.path, JSON.stringify(this.logicalToReal, null, 2));
  }
}
