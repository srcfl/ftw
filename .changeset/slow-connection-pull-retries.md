---
"forty-two-watts": patch
---

Fix docker pulls failing on slow connections at first boot and in-app update.

`firstboot.sh` capped retries at 6 attempts (~90 s total) — shorter than a single
pull on a 0.5 Mbps link. It now retries indefinitely with a 60 s gap; the sentinel
is only written on success so a reboot is always a safe abort path.

`ftw-updater` shared one 2-hour context across all 3 pull attempts, leaving almost
no room for retries after a slow-but-failed download. Each attempt now gets its own
independent 2-hour window, retries are unbounded, and `compose up -d` gets a
separate 10-minute timeout since it only recreates an already-pulled container.
