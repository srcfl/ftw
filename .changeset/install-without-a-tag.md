---
"ftw": minor
---

Install FTW on a Linux computer with one pasted line and no version to copy. Without `--tag`, `install.sh --fresh-host` takes the newest stable release, or the newest beta while no stable exists; `--beta` takes the newest beta when it is newer. The script also runs safely from `curl … | bash`. It ends with the addresses to open, one per IPv4 address, such as `http://192.168.1.23:8080/setup`, plus the `.local` name when Avahi or systemd-resolved announces one. A fresh run that finds an install cut short says to finish it with `--resume`, which now takes the release from that install. In a container or WSL without systemd, it says so before it changes anything.
