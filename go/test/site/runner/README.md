# siterig-runner

Runs FTW's site test (`go/test/site`) on a machine of its own and publishes
the result as a GitHub commit status on the exact commit it tested:

| Status | When | What runs |
|---|---|---|
| `siterig/full` | each new commit on `master` | every scenario |
| `siterig/short` | the head of each open pull request from this repository that touches `go/internal/{control,loadpoint,ocpp,telemetry}` or `go/test/site` | the short set |
| `siterig/random` | once a night on `master` | that night's random seeds |

A commit that already has the status is not run again. To run one again,
post a new `pending` status or remove the result and wait a day. No GitHub
Actions workflow runs the rig; `native-release.yml` only reads
`siterig/full`.

The description says how it went, for example `16 pass, 3 known, 0 fail of
19 in 24m`. The run directories stay in `~/siterig/results` for 14 days.

## The machine

Any Mac or Linux host with Go, git, make and the `gh` CLI that can stay
awake. A laptop works if it does not sleep: a sleeping host stretches the
rig's timing, and the rig then fails the run as "host paused". Not the home
energy box: the rig needs the CPU, and the box is production.

The rig uses 100 ports per lane from `-port-base` (default 18700) on
127.0.0.1. Leave them free.

Pull requests from forks are skipped unless `-allow-forks` is set: the rig
builds and runs the pull request's code on this machine.

## Install

```bash
gh auth login        # an account that may write commit statuses on srcfl/ftw
mkdir -p ~/siterig/bin
git clone https://github.com/srcfl/ftw ~/siterig/src
cd ~/siterig/src/go && go build -o ~/siterig/bin/siterig-runner ./test/site/runner
~/siterig/bin/siterig-runner -once     # one pass in the foreground
```

macOS (launchd):

```bash
sed "s/USER/$USER/g" ~/siterig/src/go/test/site/runner/energy.ftw.siterig-runner.plist \
  > ~/Library/LaunchAgents/energy.ftw.siterig-runner.plist
launchctl load ~/Library/LaunchAgents/energy.ftw.siterig-runner.plist
```

Linux (systemd): edit `siterig-runner.service`, copy it to
`/etc/systemd/system/` and run `sudo systemctl enable --now siterig-runner`.

The log is `~/siterig/runner.log` (launchd) or `journalctl -u
siterig-runner` (systemd). Rebuild the binary after a change to this
directory; the scenarios themselves always come from the commit under test.

## How long it takes

On an M5 MacBook (10 cores), 22 October 2026: the 23 scenarios of that
morning took 72 minutes in three lanes. With the one-phase charger cases the
set adds up to about 270 minutes, so three lanes need about 90 and four
about 70; `s12-20` alone takes up to 56, so more than four do not help.
Each lane is one `ftw` and one `simulator run`, both mostly idle.

## Flags

| Flag | Default | |
|---|---|---|
| `-repo` | `srcfl/ftw` | |
| `-work` | `~/siterig` | clone, worktrees, results, `last-random` |
| `-every` | `5m` | how often to look for new commits |
| `-lanes` | `3` | scenarios at once |
| `-port-base` | `18700` | first port |
| `-random-at`, `-random-seeds` | `2`, `10` | local hour and count of the nightly random seeds; `-random-at -1` turns them off |
| `-drafts` | `true` | also run draft pull requests |
| `-allow-forks` | `false` | also run forks' pull requests |
| `-once` | | one pass, then exit |
