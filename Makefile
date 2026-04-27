CMDS := agent-runner harness-cli harness-server harness-tui
BIN_TARGETS := $(addprefix bin/,$(CMDS))

.PHONY: all build check webui-build wasm-check test vet clean protoregen help $(BIN_TARGETS)

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

$(BIN_TARGETS): bin/%:
	@mkdir -p bin
	go build -o $@ ./cmd/$*

# Compile-check every package without producing binaries (faster than `build`).
# Useful before commit to catch breakage outside cmd/.
check: webui-build
	go build ./...

# Static check that the wasm-relevant packages still compile under
# GOOS=js GOARCH=wasm. Run before commit.
wasm-check:
	GOOS=js GOARCH=wasm go build ./cli/... ./transport/... ./cmd/harness-webui-wasm/

test:
	go test ./...

# NOTE: go vet currently exits non-zero due to pre-existing unreachable-code
# warnings in exec/frame/frame.go (bgn-generated; will be overwritten on
# regeneration). Treat exit-1 as expected unless new warnings appear in
# non-generated files.
vet:
	go vet ./...

clean:
	rm -rf bin
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
	@echo "  check         webui-build + go build ./... (compile-check, no artifacts)"
	@echo "  wasm-check    GOOS=js GOARCH=wasm go build (lint level)"
	@echo "  test          go test ./..."
	@echo "  vet           go vet ./..."
	@echo "  protoregen    regenerate Go from .bgn via brgen api server"
	@echo "  clean         remove bin/ and build artifacts"
