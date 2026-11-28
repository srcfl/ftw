---
"ftw": patch
---

The installer and the first `ftw` commands speak plainly. On a box it set up,
the install line now says FTW is already installed, prints the addresses and
points to `ftw status` and `ftw update`. After the documented
removal it names the data left behind and the commands that back it up and
clear it. It names a missing tool with the command that adds it, finds the
address without `ip` or `hostname -I`, checks port 8080 without `ss`, checks
free space before the download and unpacks it in `/var/tmp` when `/tmp` is a
small tmpfs. On macOS, a 32-bit system or a computer without systemd it points
to Raspberry Pi OS Lite (64-bit), the FTW app for Home Assistant and the
install guide. `ftw backup` and `ftw support` say that Core waits for setup
instead of printing HTTP 405 or 404.
