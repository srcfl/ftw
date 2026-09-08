---
"ftw": patch
---

Show compressed Parquet source bytes, estimated progress, throughput, elapsed time and remaining time during history import. Report checking, importing, waiting for live writes and checkpointing separately; hide estimates when there is too little evidence. Skip the full SQLite row scan when its import already has a completion receipt.
