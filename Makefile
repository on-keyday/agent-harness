CMDS := agent-runner harness-cli harness-server harness-tui
GOEXE := $(shell go env GOEXE)
BIN_TARGETS := $(addsuffix $(GOEXE),$(addprefix bin/,$(CMDS)))

.PHONY: all build release check webui-build wasm-check test vet clean protoregen help $(BIN_TARGETS)

# Per-target flags appended to `go build` in the BIN_TARGETS recipe. Empty by
# default so `make build` matches dev expectations (debug info preserved);
# `make release` sets release-style flags (see below).
BUILD_FLAGS ?=

GOROOT := $(shell go env GOROOT)
WASM_EXEC := $(GOROOT)/lib/wasm/wasm_exec.js

all: build

# Build the wasm module and refresh wasm_exec.js from the current Go SDK.
webui-build:
	GOOS=js GOARCH=wasm go build -o webui/static/main.wasm ./cmd/harness-webui-wasm/
	cp "$(WASM_EXEC)" webui/static/wasm_exec.js

# Build all native binaries into bin/. Requires webui-build first
# (cmd/harness-server uses //go:embed which needs static/main.wasm).
build: webui-build $(BIN_TARGETS)

$(BIN_TARGETS): bin/%$(GOEXE):
	@mkdir -p bin
	go build $(BUILD_FLAGS) -o $@ ./cmd/$*

# Release-style build: -trimpath (strip local paths from binaries for
# reproducibility) + -ldflags="-s -w" (strip symbol/DWARF tables, ~5MB
# smaller per binary). Used by CI's matrix build; honors GOOS / GOARCH in
# the env for cross-compile.
release: BUILD_FLAGS := -trimpath -ldflags="-s -w"
release: build

# Compile-check every package without producing binaries (faster than `build`).
# Useful before commit to catch breakage outside cmd/.
check: webui-build
	go build ./...

# Static check that the wasm-relevant packages still compile under
# GOOS=js GOARCH=wasm. Run before commit.
wasm-check:
	GOOS=js GOARCH=wasm go build ./cli/... ./cmd/harness-webui-wasm/

test:
	go test ./...

# Packages that contain only brgen-generated Go (no hand-written .go alongside).
# Excluded from vet because the generated emit can hit benign vet warnings
# (notably 'unreachable code' from the bgn-driven switches) that we don't
# want to gate CI on. Mixed packages like runner/protocol stay in vet's scope
# because their hand-written code still benefits from the check.
VET_GENERATED_PKGS := github.com/on-keyday/agent-harness/exec/frame

vet:
	@go list ./... | grep -v -F -x $(addprefix -e ,$(VET_GENERATED_PKGS)) | xargs go vet

clean:
	# Remove only the binaries; leave bin/.run/ alone so scripts/runner.sh
	# state (pid / log / shutdown sentinels) survives. `rm -rf bin` was the
	# old form; it nuked the runtime dir, leaving live processes pointing
	# at a deleted shutdown-file path that the orchestrator could no longer
	# discover (build_and_restart_all.py reports "no alive agent-runner slots").
	rm -f $(BIN_TARGETS)
	rm -f webui/static/main.wasm
	go clean ./...

# Regenerate Go from .bgn schemas via the brgen local api server.
# Default target set is runner/protocol/message.bgn only. Pass paths
# as arguments or use --all to sweep every .bgn in the repo. First
# invocation downloads ~/.cache/brgen-kit (~20 MB tarball + npm install,
# one-time ~10 s); subsequent runs are ~5 s server start + ~1 s codegen.
#   make protoregen
#   make protoregen ARGS='runner/protocol/message.bgn'
#   make protoregen ARGS=--all
protoregen:
	@./scripts/protoregen.sh $(ARGS)

help:
	@echo "Targets:"
	@echo "  webui-build   build wasm module + refresh wasm_exec.js"
	@echo "  build         webui-build + emit bin/<cmd> for each cmd/*"
	@echo "  release       build with -trimpath -ldflags=\"-s -w\" (used by CI matrix)"
	@echo "  check         webui-build + go build ./... (compile-check, no artifacts)"
	@echo "  wasm-check    GOOS=js GOARCH=wasm go build (lint level)"
	@echo "  test          go test ./..."
	@echo "  vet           go vet ./..."
	@echo "  protoregen    regenerate Go from .bgn via brgen api server"
	@echo "  clean         remove bin/ and build artifacts"
