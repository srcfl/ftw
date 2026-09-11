# Review routing

Cursor's PR Routing agent has been requesting reviewers from commit
history. That pings @erikarenhill on Discord several times a day for
agent PRs he does not need to see, and it keeps doing so whether or
not he responds.

This file overrides that reviewer-selection guidance. See
[PR Routing & Approval](https://cursor.com/docs/approval-agents).

## Who to request

Request a GitHub review only from people listed in `.github/CODEOWNERS`
for a path this PR actually changes.

Do not pick reviewers from git blame, commit history, or "who else
collaborates here."

Never request @erikarenhill. He is not a CODEOWNER. A human who
actually wants his eyes can still request him by hand.

Do not @mention people in PR bodies or comments to get their
attention. The GitHub Discord bot turns those into pings too.

## When not to request anyone

If no CODEOWNERS path matches, request nobody. Web/UI changes need a
human looking at the rendered page (see AGENTS.md); that is not a
GitHub review request.

Do not re-request a reviewer on later pushes of the same PR. Once is
enough.

## Approvals

This file does not authorise auto-approving PRs. CODEOWNERS review
and the web visual check stay as they are.
