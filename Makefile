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
        run-sim dev fmt vet clean e2e ci ci-ui ci-hw-pi docs \
        verify verify-all install-hooks

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
	@echo "  verify               pre-commit: vet + test + build (mirrors CI 'go test + vet' workflow)"
	@echo "  verify-all           pre-push: verify + cross-compile linux/arm64, linux/amd64, windows"
	@echo "  install-hooks        install git pre-commit + pre-push hooks (opt-in)"
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

# ---- Fast local verification (mirrors GitHub Actions) ----
#
# verify mirrors the "go test + vet" workflow in .github/workflows/test.yml.
# When this passes, that CI workflow is guaranteed to pass (modulo network
# deps or flakes). Keep the commands here in sync with that workflow file.
#
# verify-all adds cross-compile checks for all release targets, catching
# platform-specific syscall/import mistakes before push.

verify:
	cd go && go vet ./...
	cd go && go test ./...
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
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts ./cmd/forty-two-watts
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-pair ./cmd/ftw-pair
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-ferroamp ./cmd/sim-ferroamp
	cd go && go build -ldflags="$(LDFLAGS)" -o ../bin/sim-sungrow ./cmd/sim-sungrow
	@ls -la bin/

build-arm64:
	@mkdir -p bin
	cd go && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts-linux-arm64 ./cmd/forty-two-watts
	cd go && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-pair-linux-arm64 ./cmd/ftw-pair
	@ls -la bin/forty-two-watts-linux-arm64 bin/ftw-pair-linux-arm64

build-amd64:
	@mkdir -p bin
	cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts-linux-amd64 ./cmd/forty-two-watts
	cd go && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/ftw-pair-linux-amd64 ./cmd/ftw-pair
	@ls -la bin/forty-two-watts-linux-amd64 bin/ftw-pair-linux-amd64

build-windows-amd64:
	@mkdir -p bin
	cd go && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o ../bin/forty-two-watts-windows-amd64.exe ./cmd/forty-two-watts
	@ls -la bin/forty-two-watts-windows-amd64.exe

# ---- Release archives ----

release: build-arm64 build-amd64 build-windows-amd64
	@mkdir -p release
	@# Per-arch staging dirs so the tarballs ship forty-two-watts +
	@# ftw-pair as siblings (the `forty-two-watts pair` subcommand
	@# locates ftw-pair next to itself by default).
	@for arch in arm64 amd64; do \
		stage="bin/stage-linux-$$arch"; \
		mkdir -p "$$stage"; \
		cp "bin/forty-two-watts-linux-$$arch" "$$stage/forty-two-watts"; \
		cp "bin/ftw-pair-linux-$$arch"        "$$stage/ftw-pair"; \
		tar czf release/forty-two-watts-linux-$$arch.tar.gz \
			-C "$$stage" forty-two-watts ftw-pair \
			-C ../.. drivers web config.example.yaml; \
		printf "built release/forty-two-watts-linux-%s.tar.gz (%s bytes)\n" "$$arch" \
			"$$(wc -c <release/forty-two-watts-linux-$$arch.tar.gz)"; \
	done
	@# Windows: .zip — pair sidecar is Linux-only (uses fowld + systemctl)
	@# so we don't ship it on Windows. Delete first so rerunning release
	@# doesn't keep appending to a stale archive.
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
