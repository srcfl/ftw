# Contributing to FTW

Thanks for helping build the local energy coordination layer. This document
covers the legal bits — for how the code is organized and how to add a driver,
start with [`AGENTS.md`](AGENTS.md) and
[`docs/writing-a-driver.md`](docs/writing-a-driver.md).

## Website

The public website (<https://ftw.sourceful.energy>) lives in its own
repository, [`srcfl/ftw-web`](https://github.com/srcfl/ftw-web). Landing-page
copy, install instructions and other site content are edited there, not in this
repo — open a pull request against `srcfl/ftw-web` for website changes.

## License of contributions

This project is licensed under the **Apache License, Version 2.0** (see
[`LICENSE`](LICENSE)). By submitting a contribution, you agree that your
contribution is licensed under the Apache License, Version 2.0.

## Developer Certificate of Origin (DCO)

We use the [Developer Certificate of Origin](https://developercertificate.org/)
instead of a CLA. It is a lightweight way for you to certify that you wrote, or
otherwise have the right to submit, the code you are contributing.

Every commit must be signed off. Add a `Signed-off-by` line with your real name
and email by committing with the `-s` flag:

```bash
git commit -s -m "feat(drivers): add my new driver"
```

This appends a line like:

```
Signed-off-by: Your Name <you@example.com>
```

The full text of the DCO you are certifying against:

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.


Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

## Start with text

External code contributions start with a written proposal, not an
implementation pull request. Open an issue or Discussion before writing code
and describe:

- the problem and current behaviour;
- the result you want and what stays out of scope;
- real hardware, tariff, protocol or user evidence where it applies;
- safety and failure behaviour;
- the test, run or rendered view that would prove the change;
- related issues and open pull requests that touch the same area.

Wait for a maintainer to accept the scope in that thread. An accepted proposal
covers one focused change; it does not approve a stack of follow-on pull
requests. Once the scope is agreed, link the thread from the implementation
pull request and keep the code within it.

This review protects contributor time and keeps new work aligned with current
architecture and work already in progress. We may close an unsolicited code
pull request and invite the author to bring the proposal back as text first.
Documentation-only corrections may go straight to a pull request, but the pull
request must still explain the change and why it is correct.

## Pull requests

- Link the written proposal and the maintainer comment that accepted its scope.
- Keep PRs focused on one logical change.
- New code needs tests; `make verify` must pass before review.
- User-visible changes need a Changeset entry (`npx changeset`) — see the
  Releases section in [`README.md`](README.md).

## Betas, issues and releases

Every merged change ships in the next beta, usually within days — the beta
channel is where new work gets its real test. Run beta on your site if you
can live with the occasional rough edge; it is the most useful thing a
contributor can do.

- When you find a problem, open an issue and name the version you saw it on
  (the dashboard's update view shows it, and `GET /api/version/check`
  returns it).
- Say so in the issue when you believe the line must not promote to stable
  until it is fixed — a maintainer applies the `release-blocker` label that
  gates promotion.
- Fixes land on `master` and reach you in the next beta. A stable follows
  once a beta has run clean on the validation sites for a few days.

The repository owner cuts every release and may merge ahead of a pending
review to keep pace; the review then happens on the running beta. The full
rules, including the stable-hotfix runbook, are in the Releases section of
[`AGENTS.md`](AGENTS.md).
