# Top-level build for FTW (pure Go + Lua drivers).
#
# Common targets:
#   make test                 — full test suite
#   make build                — native binaries for this machine
#   make build-arm64          — cross-compile for linux/arm64 (RPi)
#   make build-amd64          — cross-compile for linux/amd64 (x86_64 server)
#   make build-windows-amd64  — cross-compile for windows/amd64 (.exe)
#   make release              — linux arm64/amd64 tarballs + windows zip
#   make run-sim              — start both simulators locally
#   make dev                  — start sims + main app (hot-reload workflow)
#   make clean                — remove all build artifacts

.PHONY: help test optimizer-install optimizer-test build build-arm64 build-amd64 build-windows-amd64 release \
        run-sim dev fmt vet clean e2e ci ci-ui ci-hw-pi docs \
        verify verify-all install-hooks \
        e2e-docker-up e2e-docker-logs e2e-docker-down e2e-docker-tier2

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
OPTIMIZER_PYTHON := $(CURDIR)/optimizer/.venv/bin/python
PYTHON ?= python3

help:
	@echo "FTW — Go + Lua EMS"
	@echo ""
	@echo "Targets:"
	@echo "  test                 run full test suite"
	@echo "  build                native binaries into bin/"
	@echo "  build-arm64          cross-compile for linux/arm64"
	@echo "  build-amd64          cross-compile for linux/amd64"
	@echo "  build-windows-amd64  cross-compile for windows/amd64 (.exe)"
	@echo "  release              linux tarballs + windows zip in release/"
	@echo "  run-sim              start Ferroamp + Sungrow simulators"
	@echo "  dev                  start sims + main app against config.local.yaml"
	@echo "  e2e                  run the full-stack e2e test"
	@echo "  verify               pre-commit: vet + test + build (mirrors CI 'go test + vet' workflow)"
	@echo "  verify-all           pre-push: verify + cross-compile linux/arm64, linux/amd64, windows"
	@echo "  install-hooks        install git pre-commit + pre-push hooks (opt-in)"
	@echo "  ci                   run local CI incl. browser smoke"
	@echo "  ci-ui                browser smoke against FTW_BASE_URL"
	@echo "  ci-hw-pi             deploy candidate to Pi CI slot + browser smoke"
	@echo "  fmt vet              Go format + static checks"
	@echo "  clean                nuke build artifacts"

# ---- Testing ----

test: optimizer-test
	cd go && FTW_TEST_OPTIMIZER_PYTHON=$(OPTIMIZER_PYTHON) go test ./...

optimizer-install:
	$(PYTHON) -m venv optimizer/.venv
	optimizer/.venv/bin/pip install -e 'optimizer[test]'

optimizer-test: optimizer/.venv/bin/pytest
	optimizer/.venv/bin/pytest -q optimizer/tests

optimizer/.venv/bin/pytest: optimizer/pyproject.toml
	$(MAKE) optimizer-install

e2e:
	cd go && go test ./test/e2e -v -timeout 180s

ci:
	./scripts/ci-local.sh

ci-ui:
	./scripts/ci-ui-browser.sh $${FTW_BASE_URL:-http://localhost:8080}

ci-hw-pi:
	./scripts/ci-hw-pi.sh

# ---- Fast local verification (mirrors GitHub Actions) ----
#
# verify mirrors the "go test + vet" workflow in .github/workflows/test.yml.
# When this passes, that CI workflow is guaranteed to pass (modulo network
# deps or flakes). Keep the commands here in sync with that workflow file.
#
# verify-all adds cross-compile checks for all release targets, catching
# platform-specific syscall/import mistakes before push.

verify: optimizer-test
	cd go && go vet ./...
	cd go && FTW_TEST_OPTIMIZER_PYTHON=$(OPTIMIZER_PYTHON) go test ./...
	cd go && go build ./...
	@echo "verify: vet + test + build clean"

verify-all: verify
	cd go && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
	cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...
	cd go && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
	@echo "verify-all: cross-compile clean (linux/arm64, linux/amd64, windows/amd64)"

install-hooks:
	@cp scripts/git-hooks/pre-commit .git/hooks/pre-commit
	@cp scripts/git-hooks/pre-push   .git/hooks/pre-push
	@chmod +x .git/hooks/pre-commit .git/hooks/pre-push
	@echo "git hooks installed — uninstall with: rm .git/hooks/pre-commit .git/hooks/pre-push"

# ---- Native builds ----

build:
	@mkdir -p bin
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/ftw ./cmd/ftw
	@ln -sf ftw bin/forty-two-watts
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-pair ./cmd/ftw-pair
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-relay ./cmd/ftw-relay
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-ferroamp ./cmd/sim-ferroamp
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-sungrow ./cmd/sim-sungrow
	@ls -la bin/

build-arm64:
	@mkdir -p bin
	cd go && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-linux-arm64 ./cmd/ftw
	@cp bin/ftw-linux-arm64 bin/forty-two-watts-linux-arm64
	cd go && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-pair-linux-arm64 ./cmd/ftw-pair
	cd go && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-relay-linux-arm64 ./cmd/ftw-relay
	@ls -la bin/ftw-linux-arm64 bin/forty-two-watts-linux-arm64 bin/ftw-pair-linux-arm64 bin/ftw-relay-linux-arm64

build-amd64:
	@mkdir -p bin
	cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-linux-amd64 ./cmd/ftw
	@cp bin/ftw-linux-amd64 bin/forty-two-watts-linux-amd64
	cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-pair-linux-amd64 ./cmd/ftw-pair
	cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-relay-linux-amd64 ./cmd/ftw-relay
	@ls -la bin/ftw-linux-amd64 bin/forty-two-watts-linux-amd64 bin/ftw-pair-linux-amd64 bin/ftw-relay-linux-amd64

build-windows-amd64:
	@mkdir -p bin
	cd go && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-windows-amd64.exe ./cmd/ftw
	@cp bin/ftw-windows-amd64.exe bin/forty-two-watts-windows-amd64.exe
	@ls -la bin/ftw-windows-amd64.exe bin/forty-two-watts-windows-amd64.exe

# ---- Release archives ----

RELAY_WEB_MANIFEST := web/relay-bootstrap-files.txt

relay-web:
	@rm -rf bin/ftw-relay-web
	@mkdir -p bin/ftw-relay-web release
	@while IFS= read -r f; do \
		[ -n "$$f" ] || continue; \
		case "$$f" in \#*) continue ;; esac; \
		mkdir -p "bin/ftw-relay-web/$$(dirname "$$f")"; \
		cp "web/$$f" "bin/ftw-relay-web/$$f"; \
	done < "$(RELAY_WEB_MANIFEST)"
	@COPYFILE_DISABLE=1 tar --no-xattrs -czf release/ftw-relay-web.tar.gz -C bin/ftw-relay-web .
	@cd release && shasum -a 256 ftw-relay-web.tar.gz > ftw-relay-web.tar.gz.sha256
	@printf "built release/ftw-relay-web.tar.gz (%s bytes)\n" \
		"$$(wc -c <release/ftw-relay-web.tar.gz)"

release: build-arm64 build-amd64 build-windows-amd64
	@mkdir -p release
	@# Per-arch staging dirs ship ftw, its compatibility alias, and
	@# ftw-pair as siblings (the `ftw pair` subcommand
	@# locates ftw-pair next to itself by default).
	@for arch in arm64 amd64; do \
		stage="bin/stage-linux-$$arch"; \
		mkdir -p "$$stage"; \
		cp "bin/ftw-linux-$$arch"             "$$stage/ftw"; \
		ln -sf ftw                              "$$stage/forty-two-watts"; \
		cp "bin/ftw-pair-linux-$$arch"        "$$stage/ftw-pair"; \
		tar czf release/ftw-linux-$$arch.tar.gz \
			-C "$$stage" ftw forty-two-watts ftw-pair \
			-C ../.. drivers web optimizer/pyproject.toml optimizer/ftw_optimizer config.example.yaml LICENSE NOTICE; \
		cp "release/ftw-linux-$$arch.tar.gz" "release/forty-two-watts-linux-$$arch.tar.gz"; \
		printf "built release/ftw-linux-%s.tar.gz (%s bytes)\n" "$$arch" \
			"$$(wc -c <release/ftw-linux-$$arch.tar.gz)"; \
	done
	@# Windows: .zip — pair sidecar is Linux-only (uses fowld + systemctl)
	@# so we don't ship it on Windows. Delete first so rerunning release
	@# doesn't keep appending to a stale archive.
	@rm -rf bin/stage-windows-amd64
	@mkdir -p bin/stage-windows-amd64
	@cp bin/ftw-windows-amd64.exe bin/stage-windows-amd64/ftw.exe
	@cp bin/ftw-windows-amd64.exe bin/stage-windows-amd64/forty-two-watts.exe
	@rm -f release/ftw-windows-amd64.zip release/forty-two-watts-windows-amd64.zip
	@cd bin/stage-windows-amd64 && zip -q ../../release/ftw-windows-amd64.zip ftw.exe forty-two-watts.exe
	@zip -qr release/ftw-windows-amd64.zip drivers web optimizer/pyproject.toml optimizer/ftw_optimizer config.example.yaml LICENSE NOTICE
	@cp release/ftw-windows-amd64.zip release/forty-two-watts-windows-amd64.zip
	@cd release && for f in \
		ftw-linux-arm64.tar.gz forty-two-watts-linux-arm64.tar.gz \
		ftw-linux-amd64.tar.gz forty-two-watts-linux-amd64.tar.gz \
		ftw-windows-amd64.zip forty-two-watts-windows-amd64.zip; do \
		shasum -a 256 "$$f" > "$$f.sha256"; \
	done
	@printf "built release/ftw-windows-amd64.zip (%s bytes)\n" \
		"$$(wc -c <release/ftw-windows-amd64.zip)"

# ---- Dev workflow ----

config.local.yaml: config.local.example.yaml
	@cp config.local.example.yaml config.local.yaml
	@mkdir -p dev-data
	@echo "Created config.local.yaml from the simulator template."

run-sim:
	@echo "Starting simulators (Ctrl+C to stop)..."
	@trap 'kill 0' SIGINT; \
	(cd go && go run ./cmd/sim-ferroamp) & \
	(cd go && go run ./cmd/sim-sungrow) & \
	wait

dev: optimizer/.venv/bin/pytest config.local.yaml
	@mkdir -p dev-data
	@echo "Starting sims + main app (Ctrl+C to stop)..."
	@trap 'kill 0' SIGINT; \
	(cd go && go run ./cmd/sim-ferroamp) & \
	(cd go && go run ./cmd/sim-sungrow) & \
	sleep 2 && \
	(cd go && FTW_OPTIMIZER_PYTHON=$(OPTIMIZER_PYTHON) FTW_OPTIMIZER_DIR=../optimizer go run ./cmd/ftw -config ../config.local.yaml -web ../web) & \
	wait

# ---- Local docker E2E harness (relay + Pi on this machine) ----
#
# Brings up ftw-relay + an FTW "Pi" wired to dial it, so the whole
# home-route / owner-access / pair / P2P flow runs locally with no real Pi,
# relay VM, or Cloudflare. See docs/local-e2e-docker.md.
#   Pi:         http://localhost:8080/
#   Home route: http://home.fortytwowatts.localhost/

e2e-docker-up:
	docker compose -f docker-compose.e2e.yml up --build -d
	@echo "Pi: http://localhost:8080/   Home route: http://home.fortytwowatts.localhost/"

e2e-docker-logs:
	docker compose -f docker-compose.e2e.yml logs -f

e2e-docker-down:
	docker compose -f docker-compose.e2e.yml down -v

# ---- Tier 2: container-side browser P2P + passkey proof ----
#
# Brings up relay + Pi (patched with the harness WebAuthn RP-ID) PLUS a
# headless-Chromium (Playwright) container on the SAME bridge net, and runs a
# test that: enrolls + logs in with a virtual WebAuthn authenticator, asserts
# the REAL P2P DataChannel reaches `direct` (container-to-container, no NAT),
# and makes one authenticated owner API call over it. Exits non-zero on failure.
# See docs/local-e2e-docker.md.
E2E_TIER2_COMPOSE := -f docker-compose.e2e.yml -f docker-compose.e2e-tier2.yml

e2e-docker-tier2:
	@set -e; \
	trap 'docker compose $(E2E_TIER2_COMPOSE) --profile tier2 down -v >/dev/null 2>&1 || true' EXIT; \
	docker compose $(E2E_TIER2_COMPOSE) --profile tier2 up \
		--build --abort-on-container-exit --exit-code-from playwright

# ---- Hygiene ----

fmt:
	cd go && go fmt ./...

vet:
	cd go && go vet ./...

clean:
	rm -rf bin release
	cd go && go clean

docs:
	@echo "see docs/ for:"
	@ls -1 docs/
