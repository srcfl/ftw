---
"ftw": patch
---

Run FTW 0.x in Docker with `deploy/docker`, which builds a local image from the
checksummed release package. `ftw status` no longer suggests `journalctl`,
`systemctl` or the launcher where systemd does not run, such as in a container.
