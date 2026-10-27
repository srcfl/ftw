---
"ftw": patch
---

Start the previous release once when the current release on disk is damaged and both read the same data. Before, Core failed every start until someone edited the install by hand. This lives in `ftw-launcher`, which `ftw update` does not replace: refresh it with the installer from this release, as described under "Launcher and command updates" in docs/native-beta.md.
