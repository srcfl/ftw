# Changesets

This directory holds [changeset](https://github.com/changesets/changesets)
files — one per PR — that describe the user-visible impact of a change and
the semver bump it should drive (`patch` / `minor` / `major`).

## Why we use this

Every PR that ships behavior to users needs a changeset. The release
workflow consumes accumulated changesets to:

1. Open / update a "Version Packages" PR with the bumped version in
   `package.json` and an updated `CHANGELOG.md`.
2. After that PR merges, publish and validate the exact commit on `beta`.
3. Promote the same commit to `stable`, which builds release artifacts and
   posts the announcement. The RPi installer remains independent.

No changeset → nothing to release. The "changeset check" workflow on
PRs enforces this.

## Writing a changeset

From the repo root:

```bash
pnpm changeset       # or: npx changeset
```

The CLI will ask you what bump (`patch` / `minor` / `major`) and a
summary line. It writes a markdown file under `.changeset/` that you
commit along with your change.

You can also write one by hand — drop a `.changeset/short-name.md`
with this frontmatter:

```markdown
---
"ftw": minor
---

One-line summary of the user-visible change.

Optional follow-on paragraph(s) with detail, migration notes, etc.
```

## Choosing the bump

- `patch` — bug fix, perf tweak, internal refactor, doc fix that
  affects user-visible content.
- `minor` — new driver, new feature flag, new API endpoint, new UI
  surface, expanded device support.
- `major` — breaking change (config schema rename, removed endpoint,
  removed driver capability, sign-convention change at the boundary).
  Pair with `BREAKING CHANGE:` notes in the changeset body.

## Skipping a release

If your PR genuinely doesn't need a release entry (pure CI, internal
test plumbing, README typo), add the `no-changeset` label to the PR
instead of creating an empty changeset.
