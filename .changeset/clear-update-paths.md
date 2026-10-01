---
"ftw": patch
---

Installer messages now direct users to the new 0.x setup paths and explain that 2.x and 3.x receive no more updates. The scripts still exit without changing the site. Docker's missing-version message asks for an exact published new 0.x tag instead of suggesting an old example.

Block retired Docker release workflow dispatches before checkout or registry writes, while keeping native Changesets version PRs running.
