# Changesets

This directory holds [changeset](https://github.com/changesets/changesets)
files — one per PR — that describe the user-visible impact of a change and
the version bump it should drive (`patch` or `minor`) on the native 0.x line.

The native line starts at `0.131.0`, after the old `0.130.4` tag. Existing
Docker 1.x, 2.x and 3.x tags remain immutable; Changesets on `master` must
never return to those lines. A Docker install needs the guided migration to
reach native 0.x. It cannot use its old in-app update for this change.

## Why we use this

Every PR that ships behavior to users needs a changeset. The release
workflow consumes accumulated changesets to:

1. Open / update a "Version Packages" PR with the bumped version in
   `package.json` and an updated `CHANGELOG.md`.
2. After that PR merges, dispatch `native-release.yml` from `master` for a
   `v0.X.Y-beta.N` candidate at that exact commit.
3. Test it on the home box and at least one other real site for a week.
   Promote the same commit with `source_beta` to `v0.X.Y` stable only after
   that check and with no open `release-blocker`.

Native releases use exact tags and leave GitHub `releases/latest` and Docker
`:latest` on the old 2.x line. The old `beta.yml` and `release.yml` workflows
are only for an explicit, critical 2.x safety repair.

No changeset → nothing to release. The "changeset check" workflow on
PRs enforces this.

## Writing a changeset

From the repo root:

```bash
pnpm changeset       # or: npx changeset
```

When the CLI asks for a bump, choose `patch` or `minor`, then write a
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

- `patch` — the default for bug fixes, performance changes and work that
  adds no new visible capability or user action.
- `minor` — a new visible capability or a change a user must act on, such
  as removed behavior, a changed default or new hardware support. The owner
  approves this bump in the PR. In 0.x, a minor can break compatibility, so
  explain any migration in the changeset and test it.

Do not use `major`. CI rejects it and rejects a Core package version outside
0.x. State schema, app protocol and
driver host API compatibility each have their own checks; a Core version
bump does not replace them. A breaking change still needs clear migration
notes and tests.

## Skipping a release

If your PR genuinely doesn't need a release entry (pure CI, internal
test plumbing, README typo), add the `no-changeset` label to the PR
instead of creating an empty changeset.
