// 42W Matter controller sidecar.
//
// Speaks the same WebSocket JSON-RPC contract go/internal/matter/client.go
// already implements (message_id/command/args request, result/error_code/
// error_message response) — only the backend underneath changed, from
// python-matter-server to matter.js. See go/internal/matter/client.go's
// header comment for the wire format.
//
// 42W does not commission devices itself. Devices are commissioned by
// whatever controller they shipped with, then "shared" into 42W's fabric
// via that controller's multi-admin / "share device" flow, which mints a
// one-time pairing code. The `commission` command below joins the device
// using that code and hands back a small logical node_id for the operator
// to paste into the driver's YAML config — see drivers/matter.lua.
//
// Phase 2 (`get_pairing_code` / `set_price_feed`): the inverse role — 42W
// itself is a commissionable Matter device exposing a CommodityPrice server
// endpoint (see priceserver.ts), joinable by any other Matter controller via
// the codes `get_pairing_code` returns.
//
// Phase 3 (`sync_bridge`): 42W as a Matter *bridge* — non-Matter DERs
// (inverters, batteries, EVSE chargers) appear as bridged Matter devices
// under a single Aggregator endpoint, so other Matter ecosystems can see
// their live power draw. See bridge.ts.
// Storage path must be set via the MATTER_STORAGE_PATH env var (mapped to
// matter.js's "storage.path" variable), not @matter/nodejs/config's
// defaultStoragePath setter — that setter throws NodeJsAlreadyInitializedError
// because importing @matter/main elsewhere in this file initializes the
// default Environment before this module's own top-level code would run
// (ESM hoists all imports ahead of any importing module's body).
const STORAGE_PATH = process.env.FTW_MATTER_STORAGE_PATH ?? "/data";
process.env.MATTER_STORAGE_PATH = STORAGE_PATH;

import { join } from "path";
import { ServerNode } from "@matter/main";
import { Read, Write, Invoke } from "@matter/protocol";
import { EndpointNumber, ClusterId, AttributeId } from "@matter/types";
import { WebSocket, WebSocketServer } from "ws";
import { attributeNameFor, clusterFor } from "./clusters.js";
import { NodeMap } from "./nodemap.js";
import { createPriceEndpoint, setPriceFeed, type PricePeriod } from "./priceserver.js";
import { Bridge, type BridgedDeviceInput } from "./bridge.js";

const WS_PORT = Number(process.env.FTW_MATTER_WS_PORT ?? 5580);
const MATTER_PORT = Number(process.env.FTW_MATTER_PORT ?? 5540);

interface WsRequest {
  message_id: string;
  command: string;
  args?: Record<string, unknown>;
}

interface WsResponse {
  message_id: string;
  result?: unknown;
  error_code?: string | null;
  error_message?: string;
}

function errorResponse(messageId: string, code: string, err: unknown): WsResponse {
  return {
    message_id: messageId,
    error_code: code,
    error_message: err instanceof Error ? err.message : String(err),
  };
}

async function main() {
  const priceEndpoint = createPriceEndpoint();
  const bridge = new Bridge();
  const server = await ServerNode.create(ServerNode.RootEndpoint, {
    id: "ftw-matter-controller",
    network: { port: MATTER_PORT },
    parts: [priceEndpoint, bridge.endpoint],
  });
  await server.start();

  const nodeMap = new NodeMap(join(STORAGE_PATH, "ftw-node-map.json"));

  function peerFor(logicalNodeId: number) {
    const realId = nodeMap.realFor(logicalNodeId);
    const node = server.peers.get(realId);
    if (!node) throw new Error(`node_id ${logicalNodeId} is not connected`);
    return node;
  }

  async function handleCommission(args: Record<string, unknown>) {
    const pairingCode = String(args.pairing_code ?? "");
    if (!pairingCode) throw new Error("pairing_code is required");
    const peerId = typeof args.id === "string" ? args.id : `peer-${Date.now()}`;
    const node = await server.peers.commission({ id: peerId, pairingCode });
    const realId = node.peerAddress?.nodeId;
    if (realId === undefined) throw new Error("commissioning did not return a node address");
    const logicalId = nodeMap.assign(String(realId));
    return { node_id: logicalId };
  }

  async function handleReadAttribute(args: Record<string, unknown>) {
    const logicalId = Number(args.node_id);
    const [endpoint, clusterId, attributeId] = parseAttributePath(String(args.attribute_path));
    const node = peerFor(logicalId);
    const result = await node.interaction.read(
      Read({
        attributes: [
          {
            endpointId: EndpointNumber(endpoint),
            clusterId: ClusterId(clusterId),
            attributeId: AttributeId(attributeId),
          },
        ],
      }),
    );
    for await (const chunk of result) {
      for await (const report of chunk) {
        if (report.kind === "attr-value") return report.value;
      }
    }
    throw new Error("attribute not present in read response");
  }

  async function handleWriteAttribute(args: Record<string, unknown>) {
    const logicalId = Number(args.node_id);
    const [endpoint, clusterId, attributeId] = parseAttributePath(String(args.attribute_path));
    const node = peerFor(logicalId);
    const cluster = clusterFor(clusterId);
    const attributeName = attributeNameFor(cluster, attributeId);
    await node.interaction.write(
      Write(
        Write.Attribute({
          endpoint: EndpointNumber(endpoint),
          cluster: cluster.Cluster as never,
          attributes: attributeName as never,
          value: args.value,
        }),
      ),
    );
    return null;
  }

  async function handleSendCommand(args: Record<string, unknown>) {
    const logicalId = Number(args.node_id);
    const endpoint = Number(args.endpoint_id);
    const clusterId = Number(args.cluster_id);
    const commandName = String(args.command_name ?? "");
    const node = peerFor(logicalId);
    const cluster = clusterFor(clusterId);
    const result = await node.interaction.invoke(
      Invoke({
        commands: [
          {
            endpoint: EndpointNumber(endpoint),
            cluster: cluster.Cluster as never,
            command: commandName as never,
            fields: (args.payload ?? undefined) as never,
          },
        ],
      }),
    );
    return result;
  }

  function handleListNodes() {
    return nodeMap.list();
  }

  async function handleSetPriceFeed(args: Record<string, unknown>) {
    const current = (args.current ?? null) as PricePeriod | null;
    const forecast = Array.isArray(args.forecast) ? (args.forecast as PricePeriod[]) : [];
    await setPriceFeed(priceEndpoint, current, forecast);
    return null;
  }

  function handleGetPairingCode() {
    const codes = server.state.commissioning.pairingCodes;
    return { manual_pairing_code: codes.manualPairingCode, qr_pairing_code: codes.qrPairingCode };
  }

  async function handleSyncBridge(args: Record<string, unknown>) {
    const devices = Array.isArray(args.devices) ? (args.devices as BridgedDeviceInput[]) : [];
    await bridge.sync(devices);
    return null;
  }

  async function dispatch(req: WsRequest): Promise<WsResponse> {
    const args = req.args ?? {};
    try {
      switch (req.command) {
        case "commission":
          return { message_id: req.message_id, result: await handleCommission(args) };
        case "read_attribute":
          return { message_id: req.message_id, result: await handleReadAttribute(args) };
        case "write_attribute":
          return { message_id: req.message_id, result: await handleWriteAttribute(args) };
        case "send_command":
          return { message_id: req.message_id, result: await handleSendCommand(args) };
        case "list_nodes":
          return { message_id: req.message_id, result: handleListNodes() };
        case "set_price_feed":
          return { message_id: req.message_id, result: await handleSetPriceFeed(args) };
        case "get_pairing_code":
          return { message_id: req.message_id, result: handleGetPairingCode() };
        case "sync_bridge":
          return { message_id: req.message_id, result: await handleSyncBridge(args) };
        default:
          return errorResponse(req.message_id, "unknown_command", `unknown command '${req.command}'`);
      }
    } catch (err) {
      return errorResponse(req.message_id, "command_failed", err);
    }
  }

  // Bind to loopback only. The control-plane WS protocol has no auth — it's
  // the same RPC surface that can write_attribute/send_command to any
  // commissioned device — and network_mode: host (required for mDNS) would
  // otherwise put it on the LAN unauthenticated. 42W's main app reaches it
  // over localhost anyway (it's also network_mode: host), so loopback-only
  // costs nothing for the documented deployment.
  const wss = new WebSocketServer({ port: WS_PORT, host: "127.0.0.1", path: "/ws" });
  wss.on("connection", (ws: WebSocket) => {
    ws.on("message", async (raw: Buffer) => {
      let req: WsRequest;
      try {
        req = JSON.parse(raw.toString());
      } catch {
        return; // unparseable frame — ignore, matching client.go's tolerance
      }
      const resp = await dispatch(req);
      ws.send(JSON.stringify(resp));
    });
  });

  console.log(`ftw-matter-sidecar listening: matter=${MATTER_PORT} ws=${WS_PORT}`);
}

function parseAttributePath(path: string): [number, number, number] {
  const parts = path.split("/").map(Number);
  if (parts.length !== 3 || parts.some((n) => Number.isNaN(n))) {
    throw new Error(`malformed attribute_path '${path}'`);
  }
  return [parts[0], parts[1], parts[2]];
}

main().catch((err) => {
  console.error("ftw-matter-sidecar failed to start:", err);
  process.exit(1);
});
