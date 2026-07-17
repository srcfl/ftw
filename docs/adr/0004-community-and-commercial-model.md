# ADR 0004 — Community and commercial model

- Status: Accepted (2026-07-17)
- Extends: [ADR 0002](0002-ftw-name-and-sourceful-stewardship.md)

## Context

FTW is part of Sourceful Energy and is intended to grow as a community-driven
home energy management project. Home users need a complete, useful, local-first
system they can run and improve without depending on a hosted Sourceful control
path. Sourceful also needs a clear commercial offer for organizations that
require delivery ownership, fleet operations, integrations, or contractual
support.

A non-commercial restriction would conflict with the Open Source Definition
and with the project's existing Apache-2.0 license. Apache-2.0 permits use,
modification, distribution, and commercial offerings subject to its terms. The
project therefore cannot simultaneously remain Apache-2.0 open source and
require every commercial user to purchase a license.

## Decision

FTW uses an open-core business model with a genuinely open-source Community
edition.

### FTW Community

- remains licensed under Apache-2.0;
- includes the local runtime, dashboard, optimizer interfaces, device drivers,
  public APIs, and community integrations shipped in this repository;
- is useful as a self-hosted HEMS without a mandatory Sourceful cloud service;
- is maintained by Sourceful Energy (Sourceful Labs AB) and project
  contributors;
- accepts community issues and pull requests under the existing DCO process;
- is provided as-is with community support only, without official support,
  guaranteed response times, warranties, or an SLA.

### Sourceful commercial offering

Sourceful may separately sell supported deployments and capabilities such as
integration and rollout work, fleet and managed operations, long-term
maintenance, certified device support, white-label or OEM delivery, and
contractual support or service levels.

Commercial services do not change the rights granted by Apache-2.0. A company
may use FTW Community commercially without purchasing a license. The paid offer
provides additional software or services and an accountable delivery
relationship; it is not a fee for permission to use the Community code.

New commercial-only functionality must live behind an explicit repository,
package, service, or distribution boundary and carry its own license terms. It
must not be presented as part of the Apache-2.0 Community edition.

## Consequences

- FTW can be described without qualification as open source.
- Community contributors and adopters get predictable Apache-2.0 rights.
- Sourceful cannot prevent a third party from building a commercial offering
  from the Apache-licensed code, although trademark and endorsement rules still
  apply.
- Sourceful differentiates through enterprise capabilities, managed services,
  official support, operational experience, and the FTW/Sourceful brands.
- Previously released Apache-2.0 code remains available under that license.
- Public project copy must say "community support only" rather than imply that
  commercial use requires a separate software license.

If preventing third-party commercial use becomes more important than remaining
open source, that would require a new decision and a source-available license
for future releases. It must not be described as Apache-2.0 or OSI open source.
