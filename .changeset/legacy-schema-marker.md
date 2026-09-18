---
"ftw": patch
---

Let Cores before v3.6.0-beta.1 update without the full history copy that could not finish on a Raspberry Pi. Release notes now carry a fixed legacy state-schema marker for those Cores and a second marker with the real schema, which newer Cores read to refuse downgrades. The stable release guard checks both.
