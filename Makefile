# Top-level build for FTW (Go, DuckDB and Lua drivers).
#
# Common targets:
#   make test                 — Go suites (full-stack e2e is separate)
#   make build                — native binaries for this machine
#   make build-arm64          — cross-compile for linux/arm64 (RPi)
#   make build-amd64          — cross-compile for linux/amd64 (x86_64 server)
#   make build-windows-amd64  — cross-compile for windows/amd64 (.exe)
#   make release-linux        — linux arm64/amd64 tarballs
#   make release-windows      — windows zip (UCRT64 compiler required)
#   make release              — all archives (all target compilers required)
#   make run-sim              — start both simulators locally
#   make dev                  — start sims + main app (hot-reload workflow)
#   make clean                — remove all build artifacts

.PHONY: help test compose-migration-test container-boundary-test release-workflow-test build build-arm64 build-amd64 build-windows-amd64 release release-linux release-windows \
        run-sim sim-ocpp dev fmt vet clean e2e ci ci-ui ci-hw-pi docs \
		verify verify-all install-hooks driver-repository-validate driver-versions \
        drivers drivers-present driver-versions-across-pin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)
# DuckDB is part of Core. Builds and tests must include its C bindings.
export CGO_ENABLED := 1
export VERSION
GO_TAGS := netgo,osusergo

help:
	@echo "FTW — Go + Lua EMS"
	@echo ""
	@echo "Targets:"
	@echo "  test                 run Go suites"
	@echo "  build                native binaries into bin/"
	@echo "  build-arm64          cross-compile for linux/arm64"
	@echo "  build-amd64          cross-compile for linux/amd64"
	@echo "  build-windows-amd64  cross-compile for windows/amd64 (.exe)"
	@echo "  release-linux        linux tarballs in release/"
	@echo "  release-windows      Windows zip in release/ (UCRT64 compiler)"
	@echo "  release              all archives (all target compilers required)"
	@echo "  run-sim              start Ferroamp + Sungrow + PCS simulators"
	@echo "  sim-ocpp             dial Evify OCPP chargers at a running FTW"
	@echo "  dev                  start sims + main app against config.local.yaml"
	@echo "  e2e                  run the full-stack e2e test"
	@echo "  verify               fast pre-commit: test + compose + vet + build"
	@echo "  verify-all           pre-push: verify + Linux builds, or native Windows commands"
	@echo "  install-hooks        install git pre-commit + pre-push hooks (opt-in)"
	@echo "  driver-repository-validate  build and validate unsigned driver release artifacts"
	@echo "  driver-versions      require changed Lua drivers to increase SemVer"
	@echo "  driver-versions-across-pin  same rule, asked of the pinned snapshots"
	@echo "  ci                   run local CI incl. browser smoke"
	@echo "  ci-ui                browser smoke against FTW_BASE_URL"
	@echo "  ci-hw-pi             deploy candidate to Pi CI slot + browser smoke"
	@echo "  fmt vet              Go format + static checks"
	@echo "  drivers              fetch drivers/ from the pinned device-drivers commit"
	@echo "  clean                nuke build artifacts"

# ---- Bundled drivers ----
#
# drivers/ is a snapshot of srcfl/device-drivers at the commit pinned in
# drivers/BUNDLED_SOURCE.json. It is fetched, not authored here. The Go tests
# read it, the container image copies it, and the release tarballs carry it,
# so it has to be populated before any of those run.

# Fetch the snapshot. Safe to re-run; it writes the same bytes every time.
drivers:
	bash scripts/sync-bundled-drivers.sh

# Cheap enough to run before every test invocation: when the snapshot is on
# disk this costs one ls and no network, which is every run after the first.
#
# When it is not on disk the answer is to fetch it, not to stop and explain
# how. A fresh clone or worktree has an empty drivers/ -- they are gitignored
# -- and telling four people in a row to run one specific command is a worse
# use of their afternoon than running it for them. `go test` already downloads
# its modules on a fresh checkout; this is the same bargain, once.
drivers-present:
	@ls drivers/*.lua >/dev/null 2>&1 || { \
	  echo "drivers/ is empty; fetching the snapshot pinned in drivers/BUNDLED_SOURCE.json"; \
	  $(MAKE) --no-print-directory drivers || { \
	    echo "" >&2; \
	    echo "Could not fetch the bundled drivers. They come from" >&2; \
	    echo "srcfl/device-drivers over the network and are not in git, so this" >&2; \
	    echo "needs curl, jq and a route out. Fix that and run 'make drivers'," >&2; \
	    echo "or copy drivers/*.lua from a checkout that already has them." >&2; \
	    exit 1; }; }

# ---- Testing ----

test: drivers-present
	cd go && go test -tags=$(GO_TAGS) ./...

compose-migration-test:
	bash -n scripts/enable-modular-stack.sh scripts/migrate-legacy-compose.sh scripts/install-macos.sh scripts/sync-bundled-drivers.sh scripts/check-driver-versions.sh scripts/check-debian-base.sh
	bash scripts/test-modular-compose.sh

container-boundary-test: release-workflow-test
	bash scripts/test-container-boundaries.sh

release-workflow-test:
	bash -n scripts/check-ghcr-write-access.sh scripts/test-ghcr-write-access.sh
	bash -n scripts/test-exact-image-promotion.sh scripts/build-core.sh
	bash -n scripts/github-release-by-id.sh scripts/test-github-release-by-id.sh scripts/promote-paired-latest.sh
	bash -n scripts/test-promote-paired-latest.sh
	bash scripts/test-exact-image-promotion.sh
	bash scripts/test-github-release-by-id.sh
	bash scripts/test-ghcr-write-access.sh
	bash scripts/test-promote-paired-latest.sh

e2e: drivers-present
	cd go && FTW_E2E=1 go test ./test/e2e -v -timeout 180s

driver-repository-validate: drivers-present
	cd go && go run ./cmd/ftw-driver-repository publish -unsigned -drivers ../drivers -output ../dist/driver-repository -base-url https://example.invalid/releases/download/drivers-local -repository https://github.com/srcfl/ftw

DRIVER_BASE ?= origin/master
driver-versions:
	cd go && go run ./cmd/ftw-driver-repository check-versions -repo-root .. -base $(DRIVER_BASE) -head WORKTREE

# The same rule asked of the pins rather than of this repository's history:
# a bundled driver whose bytes moved between the old pin and the new one must
# have moved its version too. Quiet when the pin has not moved.
driver-versions-across-pin:
	bash scripts/check-driver-versions.sh $(DRIVER_BASE)

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
# verify-all checks both Linux targets on Unix, and all Windows commands
# under UCRT64 on Windows. Windows CI also runs storage, backup and ACL tests.

verify: test compose-migration-test container-boundary-test release-workflow-test native-solver-test
	cd go && go vet -tags=$(GO_TAGS) ./...
	cd go && go build -tags=$(GO_TAGS) ./...
	@echo "verify: vet + test + build clean"

verify-all: verify
	@if [ "$$(go env GOHOSTOS)" = windows ]; then \
		FTW_BUILD_ALL=1 bash scripts/build-core.sh windows amd64 bin/verify-windows-amd64; \
	else \
		FTW_BUILD_ALL=1 bash scripts/build-core.sh linux arm64 bin/verify-linux-arm64 && \
		FTW_BUILD_ALL=1 bash scripts/build-core.sh linux amd64 bin/verify-linux-amd64; \
	fi
	@echo "verify-all: local platform builds clean; Windows CI checks UCRT64 builds and tests"

install-hooks:
	@cp scripts/git-hooks/pre-commit .git/hooks/pre-commit
	@cp scripts/git-hooks/pre-push   .git/hooks/pre-push
	@chmod +x .git/hooks/pre-commit .git/hooks/pre-push
	@echo "git hooks installed — uninstall with: rm .git/hooks/pre-commit .git/hooks/pre-push"

# ---- Native builds ----

build:
	bash scripts/build-core.sh "$$(go env GOHOSTOS)" "$$(go env GOHOSTARCH)" bin
	@if [ "$$(go env GOHOSTOS)" = windows ]; then \
		cp bin/ftw.exe bin/forty-two-watts.exe; \
	else ln -sf ftw bin/forty-two-watts; fi
	cd go && go build -tags=$(GO_TAGS) -ldflags="$(LDFLAGS)" -o ../bin/sim-ferroamp ./cmd/sim-ferroamp
	cd go && go build -tags=$(GO_TAGS) -ldflags="$(LDFLAGS)" -o ../bin/sim-sungrow ./cmd/sim-sungrow
	cd go && go build -tags=$(GO_TAGS) -ldflags="$(LDFLAGS)" -o ../bin/sim-pcs ./cmd/sim-pcs
	cd go && go build -tags=$(GO_TAGS) -ldflags="$(LDFLAGS)" -o ../bin/sim-ocpp ./cmd/sim-ocpp
	@ls -la bin/

build-arm64:
	bash scripts/build-core.sh linux arm64 bin/linux-arm64
	@cp bin/linux-arm64/ftw bin/ftw-linux-arm64
	@cp bin/linux-arm64/ftw-backup bin/ftw-backup-linux-arm64
	@cp bin/ftw-linux-arm64 bin/forty-two-watts-linux-arm64

build-amd64:
	bash scripts/build-core.sh linux amd64 bin/linux-amd64
	@cp bin/linux-amd64/ftw bin/ftw-linux-amd64
	@cp bin/linux-amd64/ftw-backup bin/ftw-backup-linux-amd64
	@cp bin/ftw-linux-amd64 bin/forty-two-watts-linux-amd64

# Set CC/CXX to DuckDB's MinGW GCC 14.2.0 compilers; CI installs that version.
build-windows-amd64:
	bash scripts/build-core.sh windows amd64 bin/windows-amd64
	@cp bin/windows-amd64/ftw.exe bin/ftw-windows-amd64.exe
	@cp bin/windows-amd64/ftw-backup.exe bin/ftw-backup-windows-amd64.exe
	@cp bin/ftw-windows-amd64.exe bin/forty-two-watts-windows-amd64.exe

# ---- Release archives ----

release: release-linux release-windows

release-linux: drivers-present build-arm64 build-amd64
	@mkdir -p release
	@# Per-arch staging dirs ship ftw and its compatibility alias.
	@set -e; for arch in arm64 amd64; do \
		stage="bin/stage-linux-$$arch"; \
		rm -rf "$$stage"; \
		mkdir -p "$$stage"; \
		cp "bin/ftw-linux-$$arch"             "$$stage/ftw"; \
		cp "bin/ftw-backup-linux-$$arch"      "$$stage/ftw-backup"; \
		ln -sf ftw                              "$$stage/forty-two-watts"; \
		tar czf release/ftw-linux-$$arch.tar.gz \
			-C "$$stage" ftw ftw-backup forty-two-watts \
			-C ../.. drivers web optimizer/native/bundle config.example.yaml LICENSE NOTICE LICENSING.md THIRD-PARTY-NOTICES.txt; \
		cp "release/ftw-linux-$$arch.tar.gz" "release/forty-two-watts-linux-$$arch.tar.gz"; \
		printf "built release/ftw-linux-%s.tar.gz (%s bytes)\n" "$$arch" \
			"$$(wc -c <release/ftw-linux-$$arch.tar.gz)"; \
	done
	@set -e; cd release; for f in \
		ftw-linux-arm64.tar.gz forty-two-watts-linux-arm64.tar.gz \
		ftw-linux-amd64.tar.gz forty-two-watts-linux-amd64.tar.gz; do \
		shasum -a 256 "$$f" > "$$f.sha256"; \
	done

release-windows: drivers-present build-windows-amd64
	@mkdir -p release
	@# Windows: delete first so rerunning release does not append to a stale archive.
	@rm -rf bin/stage-windows-amd64
	@mkdir -p bin/stage-windows-amd64
	@cp bin/ftw-windows-amd64.exe bin/stage-windows-amd64/ftw.exe
	@cp bin/ftw-backup-windows-amd64.exe bin/stage-windows-amd64/ftw-backup.exe
	@cp bin/ftw-windows-amd64.exe bin/stage-windows-amd64/forty-two-watts.exe
	@rm -f release/ftw-windows-amd64.zip release/forty-two-watts-windows-amd64.zip
	@cd bin/stage-windows-amd64 && zip -q ../../release/ftw-windows-amd64.zip ftw.exe ftw-backup.exe forty-two-watts.exe
	@zip -qr release/ftw-windows-amd64.zip drivers web optimizer/native/bundle config.example.yaml LICENSE NOTICE LICENSING.md THIRD-PARTY-NOTICES.txt
	@cp release/ftw-windows-amd64.zip release/forty-two-watts-windows-amd64.zip
	@set -e; cd release; for f in \
		ftw-windows-amd64.zip forty-two-watts-windows-amd64.zip; do \
		sha256sum "$$f" > "$$f.sha256"; \
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
	(cd go && go run ./cmd/sim-pcs) & \
	wait

# Charge-point client: needs a running FTW with ocpp.enabled (see
# config.local.example.yaml). Tesla Wall Connector is in the catalog but has
# no OCPP and is skipped.
sim-ocpp:
	cd go && go run ./cmd/sim-ocpp -all -plug

dev: config.local.yaml
	@mkdir -p dev-data
	@echo "Starting sims + main app (Ctrl+C to stop)..."
	@trap 'kill 0' SIGINT; \
	(cd go && go run ./cmd/sim-ferroamp) & \
	(cd go && go run ./cmd/sim-sungrow) & \
	sleep 2 && \
	(cd go && go run ./cmd/ftw -config ../config.local.yaml -web ../web) & \
	wait

# ---- Hygiene ----

fmt:
	cd go && go fmt ./...

vet:
	cd go && go vet -tags=$(GO_TAGS) ./...

clean:
	rm -rf bin release
	cd go && go clean

docs:
	@echo "see docs/ for:"
	@ls -1 docs/

# Optional proprietary worker: verify bundled artifacts and the Core boundary.
.PHONY: native-solver-check native-solver-test
native-solver-check:
	python3 optimizer/native/verify.py
	python3 -m unittest discover -s optimizer/native -p verify_test.py

native-solver-test: native-solver-check
	@binary="$$(python3 optimizer/native/verify.py --host-binary)"; \
	if [ -n "$$binary" ]; then cd go && FTW_NATIVE_SOLVER="$$binary" FTW_FORECAST_WORKER="$$binary" go test -count=1 ./internal/mpc ./internal/energyforecast ./cmd/ftw -run 'Native|RustForecastHost'; \
	else echo "Native execution tests skipped: no bundled worker for this host"; fi
