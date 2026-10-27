# siterig-runner

Runs FTW's site test (`go/test/site`) on a machine of its own and publishes
the result as a GitHub commit status on the exact commit it tested:

| Status | When | What runs |
|---|---|---|
| `siterig/full` | each new commit on `master` | every scenario |
| `siterig/short` | the head of each open pull request from this repository that touches a watched path (below) | the short set |
| `siterig/random` | once a night on `master` | that night's random seeds |

The watched paths cover the way from a reading to a command, and the rig
itself:

- `go/cmd/ftw` (control tick wiring)
- `go/internal/{control,loadpoint,ocpp,telemetry}`
- `go/internal/drivers` (driver host, command deadlines)
- `go/internal/{mpc,optimizercontract}` (plans)
- `go/internal/{config,configreload}` (limits)
- `go/internal/{cars,sitepower}`
- `drivers/` and `optimizer/native/bundle/` (the bundled drivers and solver)
- `go/test/site`

`watchedPaths` in `runner.go` is the list; `TestTouchesWatched` pins it.

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
127.0.0.1: lane k uses `port-base + 100k` up to 99 above it, so three lanes
take 18700 to 18999. Leave them free. `make siterig` uses the same default,
so on the runner's host run it with `FTW_SITE_PORT_BASE=19700`. A second
runner on one host needs its own `-port-base` and `-work`.

Pull requests from forks are skipped unless `-allow-forks` is set: the rig
builds and runs the pull request's code on this machine.

## Install

The runner keeps its clone in `~/siterig/ftw` (`-work` plus `ftw`) and
clones it there on first start if it is missing. Build the binary from the
same clone:

```bash
gh auth login        # an account that may write commit statuses on srcfl/ftw
mkdir -p ~/siterig/bin
git clone https://github.com/srcfl/ftw ~/siterig/ftw
cd ~/siterig/ftw/go && go build -o ~/siterig/bin/siterig-runner ./test/site/runner
~/siterig/bin/siterig-runner -once     # one pass in the foreground
```

To rebuild after a change to this directory:

```bash
cd ~/siterig/ftw && git pull --quiet origin master
cd go && go build -o ~/siterig/bin/siterig-runner ./test/site/runner
```

then restart the service. The scenarios themselves always come from the
commit under test.

macOS (launchd):

```bash
sed "s/USER/$USER/g" ~/siterig/ftw/go/test/site/runner/energy.ftw.siterig-runner.plist \
  > ~/Library/LaunchAgents/energy.ftw.siterig-runner.plist
launchctl load ~/Library/LaunchAgents/energy.ftw.siterig-runner.plist
```

The log is `~/siterig/runner.log`.

Linux, as a system service: replace `USER` in `siterig-runner.service`,
copy it to `/etc/systemd/system/` and run
`sudo systemctl enable --now siterig-runner`. The log is
`journalctl -u siterig-runner`.

Linux, as a user service. Root is needed once, for
`loginctl enable-linger`, which keeps the service running after you log out
and starts it at boot:

```bash
mkdir -p ~/.config/systemd/user
sed -e '/^User=/d' -e "s/USER/$USER/g" -e 's/multi-user.target/default.target/' \
  ~/siterig/ftw/go/test/site/runner/siterig-runner.service \
  > ~/.config/systemd/user/siterig-runner.service
systemctl --user daemon-reload
systemctl --user enable --now siterig-runner
sudo loginctl enable-linger "$USER"
```

The log is `journalctl --user -u siterig-runner`. Check `HOME` and `PATH`
in the unit if your home is not `/home/$USER` or Go lives elsewhere.

## How long it takes

On an M5 MacBook (10 cores), 27 October 2026: the full set took about
2 h 20 min in three lanes. `s12-20` alone took up to 56 minutes on
22 October, so more than four lanes do not help. Each lane is one `ftw` and
one `simulator run`, both mostly idle.

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
