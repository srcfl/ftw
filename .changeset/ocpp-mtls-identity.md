---
"ftw": patch
---

Bind OCPP mTLS client certificates to the charge-point identity in the URL. A certificate signed by `client_ca_file` is no longer enough to claim another charger's id: the CN or DNS SAN must match, and a per-charger password remains an additional gate when one is configured.
