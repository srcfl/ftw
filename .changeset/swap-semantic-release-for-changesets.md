---
"forty-two-watts": patch
---

Switch the release pipeline from semantic-release to Changesets.

- `.changeset/*.md` files drive the next version bump + CHANGELOG entry.
- A "Version Packages" PR opens automatically when changesets accumulate
  on master; merging it cuts the tag and runs the binaries / docker /
  rpi-image / Discord jobs unchanged.
- PRs to master are now gated on the `changeset-check` workflow — add a
  changeset with `npx changeset`, or apply the `no-changeset` label for
  pure docs / CI / chore PRs.
- Hitchhiker codename header preserved via `scripts/apply-codename.cjs`.
