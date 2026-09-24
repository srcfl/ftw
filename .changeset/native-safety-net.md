---
"ftw": minor
---

A native update now falls back on its own when the new release keeps crashing after it started: three stops without a clean shutdown within ten minutes, during its first hour, return to the previous release. `ftw update` then skips a release that failed on the box until a newer one is published; `ftw update --retry` tries it again. When Core does not start at all, `sudo -u ftw /opt/ftw/ftw-launcher -root /opt/ftw rollback` stages the previous release, and `ftw status` names that command. The launcher refuses unknown commands instead of starting Core, and refuses to write its files as root. `install.sh --refresh --tag <release>` replaces the launcher, the `ftw` command and the service definition, which updates never replace. `ftw status` and `ftw update` name a Core that is still waiting for setup, and interrupted downloads are cleaned up before the next one.
