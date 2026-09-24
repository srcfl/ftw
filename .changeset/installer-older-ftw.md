---
"ftw": patch
---

The installer now says why it refuses: an older FTW points to "Coming from an
older FTW" in the beta guide, and a login named `ftw` (common on cards from the
old image) asks for another username. The Docker files use their own project and
folder name, `ftw-local`, so they never mix with an older FTW stack.
