# Top-level build for forty-two-watts (pure Go + Lua drivers).
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

.PHONY: help test build build-arm64 build-amd64 build-windows-amd64 release \
        run-sim dev fmt vet clean e2e ci ci-ui ci-hw-pi docs

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.Version=$(VERSION)

help:
	@echo "forty-two-watts — Go + Lua EMS"
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
	@echo "  ci                   run local CI incl. browser smoke"
	@echo "  ci-ui                browser smoke against FTW_BASE_URL"
	@echo "  ci-hw-pi             deploy candidate to Pi CI slot + browser smoke"
	@echo "  fmt vet              Go format + static checks"
	@echo "  clean                nuke build artifacts"

# ---- Testing ----

test:
	cd go && go test ./...

e2e:
	cd go && go test ./test/e2e -v -timeout 180s

ci:
	./scripts/ci-local.sh

ci-ui:
	./scripts/ci-ui-browser.sh $${FTW_BASE_URL:-http://localhost:8080}

ci-hw-pi:
	./scripts/ci-hw-pi.sh

# ---- Native builds ----

build:
	@mkdir -p bin
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts ./cmd/forty-two-watts
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-ferroamp ./cmd/sim-ferroamp
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-sungrow ./cmd/sim-sungrow
	@ls -la bin/

build-arm64:
	@mkdir -p bin
	cd go && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts-linux-arm64 ./cmd/forty-two-watts
	@ls -la bin/forty-two-watts-linux-arm64

build-amd64:
	@mkdir -p bin
	cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts-linux-amd64 ./cmd/forty-two-watts
	@ls -la bin/forty-two-watts-linux-amd64

build-windows-amd64:
	@mkdir -p bin
	cd go && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts-windows-amd64.exe ./cmd/forty-two-watts
	@ls -la bin/forty-two-watts-windows-amd64.exe

# ---- Release archives ----

release: build-arm64 build-amd64 build-windows-amd64
	@mkdir -p release
	@for arch in arm64 amd64; do \
		tar czf release/forty-two-watts-linux-$$arch.tar.gz \
			-C bin forty-two-watts-linux-$$arch \
			-C .. drivers web config.example.yaml; \
		printf "built release/forty-two-watts-linux-%s.tar.gz (%s bytes)\n" "$$arch" \
			"$$(wc -c <release/forty-two-watts-linux-$$arch.tar.gz)"; \
	done
	@# Windows: .zip (native format on the platform) — binary from bin/ plus
	@# bundled drivers/web/config.example.yaml from repo root. Delete first
	@# so rerunning release doesn't keep appending to a stale archive.
	@rm -f release/forty-two-watts-windows-amd64.zip
	@cd bin && zip -q ../release/forty-two-watts-windows-amd64.zip forty-two-watts-windows-amd64.exe
	@zip -qr release/forty-two-watts-windows-amd64.zip drivers web config.example.yaml
	@printf "built release/forty-two-watts-windows-amd64.zip (%s bytes)\n" \
		"$$(wc -c <release/forty-two-watts-windows-amd64.zip)"

# ---- Dev workflow ----

run-sim:
	@echo "Starting simulators (Ctrl+C to stop)..."
	@trap 'kill 0' SIGINT; \
	(cd go && go run ./cmd/sim-ferroamp) & \
	(cd go && go run ./cmd/sim-sungrow) & \
	wait

dev:
	@echo "Starting sims + main app (Ctrl+C to stop)..."
	@trap 'kill 0' SIGINT; \
	(cd go && go run ./cmd/sim-ferroamp) & \
	(cd go && go run ./cmd/sim-sungrow) & \
	sleep 2 && \
	(cd go && go run ./cmd/forty-two-watts -config ../config.local.yaml -web ../web) & \
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
