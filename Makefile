# Top-level build for FTW (pure Go + Lua drivers).
#
# Common targets:
#   make test                 — Go + Python suites (full-stack e2e is separate)
#   make build                — native binaries for this machine
#   make build-arm64          — cross-compile for linux/arm64 (RPi)
#   make build-amd64          — cross-compile for linux/amd64 (x86_64 server)
#   make build-windows-amd64  — cross-compile for windows/amd64 (.exe)
#   make release              — linux arm64/amd64 tarballs + windows zip
#   make run-sim              — start both simulators locally
#   make dev                  — start sims + main app (hot-reload workflow)
#   make clean                — remove all build artifacts

.PHONY: help test optimizer-install optimizer-test compose-migration-test container-boundary-test build build-arm64 build-amd64 build-windows-amd64 release \
        run-sim dev fmt vet clean e2e ci ci-ui ci-hw-pi docs \
		verify verify-all install-hooks driver-repository-validate driver-versions

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
OPTIMIZER_PYTHON := $(CURDIR)/optimizer/.venv/bin/python
PYTHON ?= python3

help:
	@echo "FTW — Go + Lua EMS"
	@echo ""
	@echo "Targets:"
	@echo "  test                 run Go + Python suites"
	@echo "  build                native binaries into bin/"
	@echo "  build-arm64          cross-compile for linux/arm64"
	@echo "  build-amd64          cross-compile for linux/amd64"
	@echo "  build-windows-amd64  cross-compile for windows/amd64 (.exe)"
	@echo "  release              linux tarballs + windows zip in release/"
	@echo "  run-sim              start Ferroamp + Sungrow simulators"
	@echo "  dev                  start sims + main app against config.local.yaml"
	@echo "  e2e                  run the full-stack e2e test"
	@echo "  verify               fast pre-commit: test + compose + vet + build"
	@echo "  verify-all           pre-push: verify + cross-compile linux/arm64, linux/amd64, windows"
	@echo "  install-hooks        install git pre-commit + pre-push hooks (opt-in)"
	@echo "  driver-repository-validate  build and validate unsigned driver release artifacts"
	@echo "  driver-versions      require changed Lua drivers to increase SemVer"
	@echo "  ci                   run local CI incl. browser smoke"
	@echo "  ci-ui                browser smoke against FTW_BASE_URL"
	@echo "  ci-hw-pi             deploy candidate to Pi CI slot + browser smoke"
	@echo "  fmt vet              Go format + static checks"
	@echo "  clean                nuke build artifacts"

# ---- Testing ----

test: optimizer/.venv/.installed
	@status=0; \
	optimizer/.venv/bin/pytest -q optimizer/tests & py_pid=$$!; \
	(cd go && go test ./...) & go_pid=$$!; \
	wait $$py_pid || status=1; \
	wait $$go_pid || status=1; \
	exit $$status
	cd go && FTW_TEST_OPTIMIZER_PYTHON=$(OPTIMIZER_PYTHON) go test ./internal/mpc \
		-run 'TestExternalOptimizer(EndToEnd|PlansMultipleLoadpoints|PlansAndValidatesMultipleStorages)$$'

optimizer-install:
	$(PYTHON) -m venv optimizer/.venv
	optimizer/.venv/bin/pip install -e 'optimizer[test]'
	@touch optimizer/.venv/.installed

optimizer-test: optimizer/.venv/.installed
	optimizer/.venv/bin/pytest -q optimizer/tests

compose-migration-test:
	bash -n scripts/enable-modular-stack.sh scripts/migrate-legacy-compose.sh scripts/install-macos.sh
	bash scripts/test-modular-compose.sh

container-boundary-test:
	bash scripts/test-container-boundaries.sh

optimizer/.venv/.installed: optimizer/pyproject.toml
	$(MAKE) optimizer-install

e2e:
	cd go && FTW_E2E=1 go test ./test/e2e -v -timeout 180s

driver-repository-validate:
	cd go && go run ./cmd/ftw-driver-repository publish -unsigned -drivers ../drivers -output ../dist/driver-repository -base-url https://example.invalid/releases/download/drivers-local -repository https://github.com/srcfl/ftw

DRIVER_BASE ?= origin/master
driver-versions:
	cd go && go run ./cmd/ftw-driver-repository check-versions -repo-root .. -base $(DRIVER_BASE) -head WORKTREE

ci:
	./scripts/ci-local.sh

ci-ui:
	./scripts/ci-ui-browser.sh $${FTW_BASE_URL:-http://localhost:8080}

ci-hw-pi:
	./scripts/ci-hw-pi.sh

# ---- Fast local verification ----
#
# verify covers the fast implementation suites. Full-stack e2e and browser
# smoke remain explicit so the common local loop does not pay their startup
# cost; `make ci` runs both before handoff.
#
# verify-all adds cross-compile checks for all release targets, catching
# platform-specific syscall/import mistakes before push.

verify: test compose-migration-test container-boundary-test
	cd go && go vet ./...
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
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-backup ./cmd/ftw-backup
	@ln -sf ftw bin/forty-two-watts
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-ferroamp ./cmd/sim-ferroamp
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-sungrow ./cmd/sim-sungrow
	@ls -la bin/

build-arm64:
	@mkdir -p bin
	cd go && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-linux-arm64 ./cmd/ftw
	cd go && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-backup-linux-arm64 ./cmd/ftw-backup
	@cp bin/ftw-linux-arm64 bin/forty-two-watts-linux-arm64
	@ls -la bin/ftw-linux-arm64 bin/forty-two-watts-linux-arm64

build-amd64:
	@mkdir -p bin
	cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-linux-amd64 ./cmd/ftw
	cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-backup-linux-amd64 ./cmd/ftw-backup
	@cp bin/ftw-linux-amd64 bin/forty-two-watts-linux-amd64
	@ls -la bin/ftw-linux-amd64 bin/forty-two-watts-linux-amd64

build-windows-amd64:
	@mkdir -p bin
	cd go && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-windows-amd64.exe ./cmd/ftw
	cd go && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-backup-windows-amd64.exe ./cmd/ftw-backup
	@cp bin/ftw-windows-amd64.exe bin/forty-two-watts-windows-amd64.exe
	@ls -la bin/ftw-windows-amd64.exe bin/forty-two-watts-windows-amd64.exe

# ---- Release archives ----

release: build-arm64 build-amd64 build-windows-amd64
	@mkdir -p release
	@# Per-arch staging dirs ship ftw and its compatibility alias.
	@for arch in arm64 amd64; do \
		stage="bin/stage-linux-$$arch"; \
		mkdir -p "$$stage"; \
		cp "bin/ftw-linux-$$arch"             "$$stage/ftw"; \
		cp "bin/ftw-backup-linux-$$arch"      "$$stage/ftw-backup"; \
		ln -sf ftw                              "$$stage/forty-two-watts"; \
		tar czf release/ftw-linux-$$arch.tar.gz \
			-C "$$stage" ftw ftw-backup forty-two-watts \
			-C ../.. drivers web optimizer/pyproject.toml optimizer/ftw_optimizer config.example.yaml LICENSE NOTICE; \
		cp "release/ftw-linux-$$arch.tar.gz" "release/forty-two-watts-linux-$$arch.tar.gz"; \
		printf "built release/ftw-linux-%s.tar.gz (%s bytes)\n" "$$arch" \
			"$$(wc -c <release/ftw-linux-$$arch.tar.gz)"; \
	done
	@# Windows: delete first so rerunning release does not append to a stale archive.
	@rm -rf bin/stage-windows-amd64
	@mkdir -p bin/stage-windows-amd64
	@cp bin/ftw-windows-amd64.exe bin/stage-windows-amd64/ftw.exe
	@cp bin/ftw-backup-windows-amd64.exe bin/stage-windows-amd64/ftw-backup.exe
	@cp bin/ftw-windows-amd64.exe bin/stage-windows-amd64/forty-two-watts.exe
	@rm -f release/ftw-windows-amd64.zip release/forty-two-watts-windows-amd64.zip
	@cd bin/stage-windows-amd64 && zip -q ../../release/ftw-windows-amd64.zip ftw.exe ftw-backup.exe forty-two-watts.exe
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

dev: optimizer/.venv/.installed config.local.yaml
	@mkdir -p dev-data
	@echo "Starting sims + main app (Ctrl+C to stop)..."
	@trap 'kill 0' SIGINT; \
	(cd go && go run ./cmd/sim-ferroamp) & \
	(cd go && go run ./cmd/sim-sungrow) & \
	sleep 2 && \
	(cd go && FTW_OPTIMIZER_PYTHON=$(OPTIMIZER_PYTHON) FTW_OPTIMIZER_DIR=../optimizer go run ./cmd/ftw -config ../config.local.yaml -web ../web) & \
	wait

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
