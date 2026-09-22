.DEFAULT_GOAL := help

GO ?= go
PACKAGE := ./cmd/tailge
BINARY ?= tailge
OUT_DIR ?= dist
FUZZTIME ?= 3s
GO_FILES := $(shell find cmd internal -type f -name '*.go' -print)

.PHONY: help format format-check tidy setup build run tui scan status doctor test race vet lint security fuzz check quality cross-build clean

help:
	@printf '%s\n' \
		'tailge development targets:' \
		'  make format        Format Go sources' \
		'  make check         Format check, tests, race tests, and vet' \
		'  make lint          Run staticcheck and golangci-lint' \
		'  make security       Run govulncheck' \
		'  make fuzz           Run bounded parser fuzz campaigns' \
		'  make quality        Run check, lint, security, and fuzz' \
		'  make setup         Build and install tailge in ~/.local/bin' \
		'  make build         Build ./tailge' \
		'  make tui           Build and launch the interactive TUI' \
		'  make run ARGS=...  Run tailge with arguments' \
		'  make scan ARGS=... Run tailge scan' \
		'  make status        Run exposure status (ARGS are forwarded)' \
		'  make doctor ARGS=... Run the doctor command' \
		'  make cross-build   Build Linux amd64 and Darwin arm64 binaries' \
		'  make clean         Remove local build artifacts'

format:
	$(GO)fmt -w $(GO_FILES)

format-check:
	@files="$$( $(GO)fmt -l $(GO_FILES) )"; \
	if test -n "$$files"; then \
		printf 'Unformatted Go files:\n%s\n' "$$files"; \
		exit 1; \
	fi

tidy:
	$(GO) mod tidy

setup:
	@mkdir -p "$(HOME)/.local/bin"
	$(GO) build -o "$(HOME)/.local/bin/$(BINARY)" $(PACKAGE)
	@case ":$${PATH}:" in \
		*":$(HOME)/.local/bin:"*) ;; \
		*) printf 'Add %s to PATH to run tailge directly.\n' "$(HOME)/.local/bin" ;; \
	esac

build: $(BINARY)

$(BINARY): $(GO_FILES) go.mod go.sum
	$(GO) build -o "$@" $(PACKAGE)

run:
	$(GO) run $(PACKAGE) $(ARGS)

tui: build
	./$(BINARY)

scan:
	$(GO) run $(PACKAGE) scan $(ARGS)

status:
	$(GO) run $(PACKAGE) exposure status $(ARGS)

doctor:
	$(GO) run $(PACKAGE) doctor $(ARGS)

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

lint:
	@command -v staticcheck >/dev/null 2>&1 || { printf '%s\n' 'staticcheck is required; install it or run make check.' >&2; exit 1; }
	staticcheck ./...
	@command -v golangci-lint >/dev/null 2>&1 || { printf '%s\n' 'golangci-lint is required for make lint.' >&2; exit 1; }
	golangci-lint run ./...

security:
	@command -v govulncheck >/dev/null 2>&1 || { printf '%s\n' 'govulncheck is required for make security.' >&2; exit 1; }
	govulncheck ./...

fuzz:
	$(GO) test ./internal/config -run '^$$' -fuzz '^FuzzParseNeverPanics$$' -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/discovery -run '^$$' -fuzz '^FuzzParseLsofNeverPanics$$' -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/discovery -run '^$$' -fuzz '^FuzzParseSSNeverPanics$$' -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/target -run '^$$' -fuzz '^FuzzParseTargetNeverPanics$$' -fuzztime=$(FUZZTIME)
	$(GO) test ./internal/tailscale -run '^$$' -fuzz '^FuzzParseStatusNeverPanics$$' -fuzztime=$(FUZZTIME)

check: format-check test race vet

quality: check lint security fuzz

cross-build:
	@mkdir -p "$(OUT_DIR)"
	GOOS=linux GOARCH=amd64 $(GO) build -o "$(OUT_DIR)/tailge-linux-amd64" $(PACKAGE)
	GOOS=darwin GOARCH=arm64 $(GO) build -o "$(OUT_DIR)/tailge-darwin-arm64" $(PACKAGE)

clean:
	rm -f "$(BINARY)"
	rm -rf "$(OUT_DIR)"
