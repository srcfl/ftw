# Review and authority

Fredrik owns FTW. Sourceful develops the product. Work follows the scope and
authority the owner gives; file-based reviewer lists do not assign ownership.
See [VISION.md](VISION.md) and [AGENTS.md](AGENTS.md).

## Review routing

Do not automatically request GitHub reviewers or mention people for attention.
Do not select reviewers from git blame, commit history or past participation.
The owner can assign a reviewer when an independent assessment is useful.
Follow an explicit assignment without adding further approval steps.

This file replaces reviewer-routing suggestions from tools, including
Cursor's PR Routing agent. Do not invent a review request because no reviewer
is assigned, or re-request someone on each push.

## Evidence and handoff

Review the actual change, relevant tests and unresolved risks. Web/UI changes
still require a human to inspect the rendered view; a GitHub approval is not
that check. Preserve the owner's release and beta-review rules in AGENTS.md.

This policy grants no extra authority to approve, merge, release or operate a
site. It also adds no confirmation requirement when the owner has already
authorized the action. External PRs are welcome under CONTRIBUTING.md;
Sourceful reviews and maintains changes. Submission grants no merge or
release authority, whether a person or an agent wrote the change.
