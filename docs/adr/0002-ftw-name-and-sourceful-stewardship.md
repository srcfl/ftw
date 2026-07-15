# ADR 0002 — FTW name and Sourceful stewardship

- Status: Accepted (2026-07-15)
- Supersedes: active product naming and stewardship statements that describe
  the project as `forty-two-watts`, `42W`, or as unaffiliated with a company
- Follow-up: ADR 0003 will govern remote-domain and WebAuthn RP-ID migration

## Context

The project currently uses several public identities: `forty-two-watts` in the
repository, Go module, command, container images and operator documentation;
`42W` in product copy and Raspberry Pi assets; and `FTW` in newer components and
environment variables.

Sourceful Labs AB is the project steward. The active project identity and
distribution paths need to reflect that stewardship without changing the
local-first architecture or breaking deployed installations.

Some legacy strings are presentation copy, while others are durable external
contracts. In particular, Home Assistant MQTT identities, Nova identifiers,
Compose project/service names, persisted state, hostnames and WebAuthn RP IDs
cannot safely be treated as ordinary brand text.

## Decision

The canonical project name is **FTW**. FTW has no official expansion.

Use the following copy:

> **Local-first home energy coordination.**

> **FTW is an open-source local energy runtime for solar, batteries, grid and EV charging.**

> **FTW is maintained by Sourceful Labs AB and project contributors.**

Where separation from Sourceful-hosted products matters, use:

> **FTW is a self-hosted open-source project maintained by Sourceful Labs AB. It is not a hosted Sourceful service, and the local control path does not depend on Sourceful cloud services.**

The target repository and Go module are `srcfl/ftw` and
`github.com/srcfl/ftw/go`. The canonical command and binary are `ftw`.
Environment variables retain the existing `FTW_*` prefix.

The migration is staged:

1. **Programme A** changes active project identity and canonical distribution
   while retaining compatibility aliases and stable integration identifiers.
2. **Programme B** separately migrates remote origins and passkeys after a
   security review. Programme A does not change the current RP ID.

Every legacy reference is classified before editing:

1. presentation identity — rename during Programme A;
2. canonical distribution identity — introduce and prove before cutover;
3. compatibility alias — retain for the declared compatibility window;
4. stable integration/wire identity — retain until a versioned migration exists;
5. security identity — migrate only in Programme B;
6. historical or third-party reference — preserve.

The compatibility window lasts for at least two published releases and at
least 90 days, using the longer period. It begins with the first canonical
release from `srcfl/ftw`.

## Consequences

- Active UI, README, website and human-readable metadata will use FTW.
- Existing configuration, databases, device IDs and energy history do not
  require a brand migration.
- Existing install directories, Compose identities and hostnames remain valid.
- Home Assistant display names may change, but topics and unique IDs do not.
- Existing Nova hardware/gateway identifiers do not change.
- The Apache-2.0 license remains unchanged; active MIT project-license claims
  are corrected while accurate third-party and prior-history references remain.
- Historical changelog entries and archives are not rewritten for appearance.
- The repository transfer and public-domain cutovers remain manual operational
  checkpoints with explicit rollback plans.

## Remote-access boundary

Programme A retains `home.fortytwowatts.com` as the WebAuthn RP ID. Relay and
TURN references move to `relay.ftw.sourceful.energy` and
`turn.ftw.sourceful.energy` now; provisioning those endpoints is a follow-up. A
new passkey origin cannot be treated as a copy-only change: WebAuthn credentials
and browser data in cookies, localStorage and IndexedDB are origin-scoped.

ADR 0003 must define related-origin behaviour, credential RP-ID storage,
browser-local identity recovery, dual-domain operation and retirement criteria
before any remote default changes.

## Detailed implementation specification

The phased work, compatibility rules, tests and release checkpoints live in
[`../superpowers/specs/2026-07-13-ftw-rebrand-and-sourceful-migration.md`](../superpowers/specs/2026-07-13-ftw-rebrand-and-sourceful-migration.md).
