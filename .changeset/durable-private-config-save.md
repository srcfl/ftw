---
"ftw": patch
---

A settings save now survives a power cut, and stops leaving the config world-readable. `config.yaml` is the file the gateway boots from, and every save from the UI rewrote it with no fsync of the temp file and no fsync of the containing directory — a rename is only atomic for bytes that already reached the disk, so losing power mid-save could publish a truncated or zero-length config and leave an unattended gateway unbootable. The save now fsyncs the temp file, renames, then fsyncs the directory, and reports a failure instead of claiming a config was saved that the next power cut can still take away. The file is also written 0600 rather than 0644: it holds MQTT passwords, API keys and OAuth refresh tokens.
