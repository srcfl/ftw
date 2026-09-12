# Contributing to FTW

Sourceful develops and maintains FTW; Fredrik owns its direction. External
PRs are welcome, including code, drivers, documentation and website changes.
Prefer an [issue](https://github.com/srcfl/ftw/issues) that states the need and
evidence, and link it from the PR. Small fixes do not need a separate issue.
The [product vision](VISION.md) guides scope and priority.

For a broad product or architecture idea, a short Markdown PR is a useful
start. Explain the problem, proposed result and how we could test it in the
PR description or relevant maintained docs. Mark proposals as proposed;
keep agent scratch notes and task breakdowns out of the repository. Concrete
fixes can include code and tests. No proposal promises a delivery date.

## Agentic first

People and agents should be able to assess and continue the work from its
record. State the problem, scope, relevant issue, test steps and results.
Use focused changes and reproducible evidence. The submitter checks agent
output and remains responsible for the contribution and its sign-off.

Test evidence should fit the change. For device behaviour, include hardware
model, firmware, test steps and observed results before merge. State missing
hardware tests and keep the PR as a proposal or draft until they are done.
Simulator tests help catch bugs but do not prove physical support. Docs do
not need hardware tests; UI changes need a browser review. Driver control
keeps its separate safety and hardware acceptance gates.

This policy changes no license, past attribution or release authority.

## Website

The public website (<https://ftw.energy>) lives in its own
repository, [`srcfl/ftw-web`](https://github.com/srcfl/ftw-web). Landing-page
copy, install instructions and other site content live there. Use
[its issue tracker](https://github.com/srcfl/ftw-web/issues) and PRs for website
changes. Shared Lua drivers belong in
[`srcfl/device-drivers`](https://github.com/srcfl/device-drivers).

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

## Report a need or bug

Describe the result you expected, what happened and the version and equipment
involved. Add relevant logs, screenshots or protocol documents when available.
Do not post credentials or private site data. You do not need to design an API
or write a patch to report a problem.

Fredrik sets direction and priority; Sourceful reviews and maintains changes.
Link related issues and PRs so the evidence stays easy to follow. General
questions can use Discussions or Discord.

## Pull requests

- State the household need, selected scope and relevant issue or owner request.
- Keep the change focused and coordinate overlap with open work.
- Test changed behaviour and failure paths; run the relevant repository checks.
- Review changed UI in a browser.
- Add a Changeset for user-visible runtime changes; documentation is exempt.
- Keep VISION.md, the roadmap and current-behaviour docs consistent.

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
