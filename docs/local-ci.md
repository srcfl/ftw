# Local CI and Raspberry Pi UI Smoke

This repo has a local CI path for fast confidence before deploy, plus a
Raspberry Pi candidate slot for checking the real served UI against live
read-only data.

## Local full pass

```bash
make ci
```

This runs:

- `make test`
- `make e2e`
- native build via `make build`
- linux/arm64 build via `make build-arm64`
- a temporary local stack with the bundled simulators
- a headless browser smoke against `/`, `/legacy`, `/setup`, and key `GET /api/*`

Artifacts land under `artifacts/local-ci/<timestamp>/`, including logs,
API JSON, DOM dumps, and screenshots.

Set `FTW_CI_SKIP_BROWSER=1` to skip the browser smoke when you only want
Go/build verification.

## Browser smoke against any running instance

```bash
make ci-ui FTW_BASE_URL=http://localhost:8080
```

or directly:

```bash
scripts/ci-ui-browser.sh http://192.168.192.40:18080
```

The script requires a local Chrome, Chromium, or Edge binary. If it is not
auto-detected, set:

```bash
BROWSER_BIN="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  scripts/ci-ui-browser.sh http://localhost:8080
```

## Raspberry Pi candidate slot

```bash
make ci-hw-pi
```

Defaults:

- SSH target: `fredde@192.168.192.40`
- remote dir: `~/forty-two-watts-ci`
- candidate URL: `http://192.168.192.40:18080`
- upstream live API from the Pi: `http://127.0.0.1:8080`

Override as needed:

```bash
FTW_PI_HOST=fredde@192.168.1.40 \
FTW_PI_PORT=18080 \
FTW_PI_UPSTREAM=http://127.0.0.1:8080 \
make ci-hw-pi
```

The Pi candidate uses `config.hw-ci.yaml` with `drivers: []` and starts
with:

```bash
FTW_PROXY_UPSTREAM=http://127.0.0.1:8080
FTW_PROXY_READONLY=1
```

That means the candidate serves its own binary and `web/` assets, while
`GET /api/*` reads from the live instance. Non-read API methods are blocked
with HTTP 403, so the candidate UI cannot save config, change mode, reset
models, or send control commands to the live system.

The script leaves the candidate process running for manual browser testing.
Tail its log with:

```bash
ssh fredde@192.168.192.40 'tail -f ~/forty-two-watts-ci/ci.log'
```

To skip the local test phase when you only want to redeploy the current
arm64 candidate:

```bash
FTW_SKIP_LOCAL=1 make ci-hw-pi
```
