/* Generates webapp-vectors.json from the reference implementation.
 *
 * The Go code in this package and the TypeScript in srcfl/ftw-webapp are two
 * independent implementations of one wire. Round-trip tests inside either one
 * only prove it agrees with itself, so this drives the TypeScript and freezes
 * its bytes; appwire's interop_test.go then replays them. When the two drift,
 * that test fails instead of a house failing to connect.
 *
 * Keys and payloads are fixed, not random, so regenerating produces the same
 * file and a diff means a real change to the wire.
 *
 * Driven by run-generator.mjs, which supplies the app's Vite server so the
 * $lib alias and the app's crypto packages resolve exactly as they do in a
 * browser build:
 *
 *   node go/internal/appwire/testdata/run-generator.mjs ../ftw-webapp
 */

import { writeFileSync } from 'node:fs'
import * as noise from '$lib/crypto/noise'
import { NoiseTransport } from '$lib/crypto/transport'
import { encodeFrame, LANE_CONTROL, LANE_BULK, FLAG_TRUNC } from '$lib/protocol/frame'

const { HandshakeState, PROTOCOL_NAME, keyPairFromSecret } = noise

/* Local, so every import here goes through the $lib alias. Anything else has
 * to resolve against the app's node_modules from a file that does not live
 * there. */
const hexToBytes = (hex: string) => Uint8Array.from(Buffer.from(hex, 'hex'))
const bytesToHex = (bytes: Uint8Array) => Buffer.from(bytes).toString('hex')

/* Fixed test keys. Not secret, never used off a test bench. */
const APP_STATIC = 'a1a1a1a1a1a1a1a1b2b2b2b2b2b2b2b2c3c3c3c3c3c3c3c3d4d4d4d4d4d4d4d4'
const APP_EPHEMERAL = '1111111111111111222222222222222233333333333333334444444444444444'
const BOX_STATIC = '5f5f5f5f5f5f5f5f6060606060606060717171717171717182828282828282f2'
const BOX_EPHEMERAL = 'aaaaaaaaaaaaaaaabbbbbbbbbbbbbbbbccccccccccccccccdddddddddddddddd'
const PROLOGUE = '66747731' // "ftw1"

/**
 * Satisfies both shapes of the reference's static key: the raw KeyPair the
 * app took when these vectors were first cut, and the async StaticKey it is
 * moving to for non-extractable WebCrypto handles. Which one is checked out
 * must not change a byte, so the generator accommodates both rather than
 * pinning one.
 */
function staticKey(secretHex: string) {
  const pair = keyPairFromSecret(hexToBytes(secretHex))
  const wrapped = typeof noise.staticFromKeyPair === 'function' ? noise.staticFromKeyPair(pair) : {}
  return { ...wrapped, ...pair }
}

function ephemeral(secretHex: string) {
  return keyPairFromSecret(hexToBytes(secretHex))
}

const utf8 = (s: string) => Uint8Array.from(new TextEncoder().encode(s))

/* Envelopes chosen to exercise what CBOR actually has to survive: negative
 * integers (export is negative W), nested maps, a string id, an envelope with
 * no body, a type neither side has heard of, and unknown keys beside a known
 * one. */
const FRAMES = [
  { name: 'tick', lane: LANE_CONTROL, flags: 0, bucket: 512, envelope: { t: 'tick', b: { seq: 41 } } },
  { name: 'tick_no_body', lane: LANE_CONTROL, flags: 0, bucket: 512, envelope: { t: 'tick' } },
  {
    name: 'delta_export',
    lane: LANE_CONTROL,
    flags: 0,
    bucket: 512,
    // Negative battery_w and grid_w: power out of the site. The sign is the
    // protocol's, and a codec that loses it loses the direction of energy.
    envelope: { t: 'delta', b: { seq: 42, fields: { 2: -4200, 3: 6200, 4: -1500, 5: 875, 6: 13400 } } },
  },
  {
    name: 'delta_truncated',
    lane: LANE_CONTROL,
    flags: FLAG_TRUNC,
    bucket: 512,
    envelope: { t: 'delta', b: { seq: 43, fields: { 2: 11400 } } },
  },
  {
    name: 'cmd_with_id',
    lane: LANE_CONTROL,
    flags: 0,
    bucket: 512,
    envelope: {
      t: 'cmd',
      id: 7,
      b: {
        cmdId: '018f3a2b-0000-7000-8000-000000000001',
        op: 'set_mode',
        args: { mode: 'idle' },
        notValidAfterMs: 1712345678901,
        expect: { rev: 12, guards: [] },
      },
    },
  },
  {
    name: 'unknown_type_with_unknown_keys',
    lane: LANE_CONTROL,
    flags: 0,
    bucket: 512,
    // An older box talking to a newer app, or the reverse. Neither the type
    // nor the extra keys may be fatal; a hard version wall turns a pinned
    // service worker into a white screen.
    envelope: { t: 'plan.v2', b: { horizonMs: 3600000 }, futureField: 'from a newer peer', anotherOne: 42 },
  },
  {
    name: 'small_control_bucket',
    lane: LANE_CONTROL,
    flags: 0,
    bucket: 256,
    envelope: { t: 'tick', b: { seq: 44 } },
  },
  {
    name: 'snap_bulk',
    lane: LANE_BULK,
    flags: 0,
    bucket: 1024,
    envelope: {
      t: 'snap',
      id: 3,
      b: {
        rev: 12,
        dict: { 2: { name: 'grid_w', unit: 'W', srcId: 'meter.p1' } },
        fields: { 2: -4200, 5: 875 },
        sources: { 'meter.p1': { kind: 'meter', lastOkMs: 900, staleAfterMs: 5000, state: 'live' } },
      },
    },
  },
] as const

/* Alternating traffic, so both directions' counters advance independently and
 * a Go implementation that shares one counter fails here. */
const TRANSPORT_SCRIPT = [
  { from: 'app', frame: 'cmd_with_id' },
  { from: 'box', frame: 'tick' },
  { from: 'box', frame: 'delta_export' },
  { from: 'box', frame: 'delta_truncated' },
  { from: 'app', frame: 'tick_no_body' },
  { from: 'box', frame: 'snap_bulk' },
  { from: 'box', frame: 'tick' },
] as const

export async function generate(target: string): Promise<void> {
  const app = staticKey(APP_STATIC)
  const box = staticKey(BOX_STATIC)

  const initiator = HandshakeState.initiator({
    staticKey: app,
    remoteStatic: box.publicKey,
    prologue: hexToBytes(PROLOGUE),
    ephemeral: ephemeral(APP_EPHEMERAL),
  })
  const responder = HandshakeState.responder({
    staticKey: box,
    prologue: hexToBytes(PROLOGUE),
    ephemeral: ephemeral(BOX_EPHEMERAL),
  })

  const message1Payload = utf8('pair=QR-8842')
  const message1 = await initiator.writeMessage(message1Payload)
  const readBack1 = await responder.readMessage(message1)

  const message2Payload = utf8('boot=ok')
  const message2 = await responder.writeMessage(message2Payload)
  const readBack2 = await initiator.readMessage(message2)

  if (bytesToHex(readBack1) !== bytesToHex(message1Payload)) throw new Error('message 1 payload mismatch')
  if (bytesToHex(readBack2) !== bytesToHex(message2Payload)) throw new Error('message 2 payload mismatch')

  const appSide = new NoiseTransport(initiator.split())
  const boxSide = new NoiseTransport(responder.split())

  const frames = FRAMES.map((f) => {
    const bytes = encodeFrame({ lane: f.lane, flags: f.flags, envelope: f.envelope }, f.bucket)
    return {
      name: f.name,
      lane: f.lane,
      flags: f.flags,
      bucket: f.bucket,
      // The declared payload length; everything past it is padding.
      payloadLen: (bytes[4]! << 8) | bytes[5]!,
      // Which keys the envelope carried. A peer drops the ones it does not
      // know, so it cannot re-encode such a frame byte for byte; the Go test
      // reads this to know which comparison is fair.
      envelopeKeys: Object.keys(f.envelope),
      bytes: bytesToHex(bytes),
    }
  })

  const byName = new Map(frames.map((f) => [f.name, f]))

  const transport = TRANSPORT_SCRIPT.map((step) => {
    const frame = byName.get(step.frame)!
    const sender = step.from === 'app' ? appSide : boxSide
    const receiver = step.from === 'app' ? boxSide : appSide
    const seq = sender.nextSeq
    const wire = sender.encrypt(hexToBytes(frame.bytes))

    // Prove the vector is one a peer can actually read, not just one this
    // file produced.
    receiver.decrypt(wire)

    return { from: step.from, frame: step.frame, seq: Number(seq), wire: bytesToHex(wire) }
  })

  const out = {
    note: 'Generated by generate-vectors.ts from srcfl/ftw-webapp. Do not hand-edit.',
    protocolName: PROTOCOL_NAME,
    handshake: {
      prologue: PROLOGUE,
      initiatorStatic: APP_STATIC,
      initiatorStaticPublic: bytesToHex(app.publicKey),
      initiatorEphemeral: APP_EPHEMERAL,
      responderStatic: BOX_STATIC,
      responderStaticPublic: bytesToHex(box.publicKey),
      responderEphemeral: BOX_EPHEMERAL,
      message1Payload: bytesToHex(message1Payload),
      message1: bytesToHex(message1),
      message2Payload: bytesToHex(message2Payload),
      message2: bytesToHex(message2),
      handshakeHash: bytesToHex(appSide.handshakeHash),
    },
    frames,
    transport,
  }

  writeFileSync(target, JSON.stringify(out, null, 2) + '\n')
}
