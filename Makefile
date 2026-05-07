.DEFAULT_GOAL := all

BINDIR      := bin
BINARY      := $(BINDIR)/poe-acp
NOTICE_FILE := THIRD_PARTY_NOTICES.md
GOBIN       := $(shell go env GOPATH)/bin
VERSION     := $(shell cat VERSION 2>/dev/null || echo dev)

# Append -dev+<sha>[.dirty] unless HEAD is the exact release tag.
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null)
GIT_TAG    := $(shell git describe --exact-match --tags HEAD 2>/dev/null)
GIT_DIRTY  := $(shell git diff --quiet 2>/dev/null || echo .dirty)
ifneq ($(GIT_TAG),v$(VERSION))
  ifneq ($(GIT_COMMIT),)
    VERSION := $(VERSION)-dev+$(GIT_COMMIT)$(GIT_DIRTY)
  endif
endif

LDFLAGS := -s -w -X main.version=$(VERSION)

GO_LICENSES := go run github.com/google/go-licenses@v1.6.0
COVGATE     := go run github.com/kfet/covgate/cmd/covgate@v0.1.0

# Cross-compile matrix. Element format: <goos>-<goarch>[-<goarm>].
CROSS      := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 linux-armv6
CROSS_BINS := $(addprefix $(BINARY)-,$(CROSS))

# ---------------------------------------------------------------------------
# Quiet step helper: $(call RUN,label,command). V=1 for verbose output.
# ---------------------------------------------------------------------------
ifdef V
  define RUN
	@printf "  %-28s\n" "$(1)"
	$(2)
  endef
else
  define RUN
	@_log=$$(mktemp) && ( $(2) ) > $$_log 2>&1 \
		&& { printf "  %-28s ✓\n" "$(1)"; rm -f $$_log; } \
		|| { printf "  %-28s ✗\n" "$(1)"; cat $$_log; rm -f $$_log; exit 1; }
  endef
endif

.PHONY: all _parallel build build-all install fmt tidy vet \
        test test-race test-cover open-coverage \
        clean notices check-licenses publish deploy

# ---------------------------------------------------------------------------
# Top-level: bare `make` runs the full CI pipeline.
# fmt + tidy run serially (they mutate files); the rest run in parallel
# via a recursive sub-make so internal -j works without the caller
# passing -j on the command line.
# ---------------------------------------------------------------------------
all: fmt tidy
	@$(MAKE) -j --no-print-directory _parallel

_parallel: vet test-race build build-all check-licenses

fmt:
	@gofmt -s -w .

tidy:
	@go mod tidy

vet:
	$(call RUN,vet,go vet ./...)

$(BINDIR):
	@mkdir -p $@

# ---------------------------------------------------------------------------
# Builds
# ---------------------------------------------------------------------------

build: | $(BINDIR)
	$(call RUN,build (native),go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/poe-acp/)

build-all: $(CROSS_BINS)

# Pattern rule for cross-compiled binaries: bin/poe-acp-<os>-<arch>.
# armv6 is special-cased to GOARCH=arm GOARM=6.
define cross_build
	$(call RUN,build $(1)/$(2),GOOS=$(1) GOARCH=$(if $(filter armv6,$(2)),arm,$(2)) $(if $(filter armv6,$(2)),GOARM=6) go build -trimpath -ldflags="$(LDFLAGS)" -o $@ ./cmd/poe-acp/)
endef

$(BINARY)-%: | $(BINDIR)
	$(call cross_build,$(word 1,$(subst -, ,$*)),$(word 2,$(subst -, ,$*)))

install:
	go install -ldflags="$(LDFLAGS)" ./cmd/poe-acp/

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

# Plain quick run.
test:
	go test ./...

# Full run: race + shuffle + 100% coverage gate (paths in .covignore excluded).
# This is what `make all` exercises.
test-race: | $(BINDIR)
	$(call RUN,test (race),\
		go test -race -shuffle=on -cover ./... -coverprofile=$(BINDIR)/coverage.tmp.out \
		&& $(COVGATE) -profile=$(BINDIR)/coverage.tmp.out -out=$(BINDIR)/coverage.out -ignore=.covignore -min=100)

# Human-friendly per-function coverage summary (no gate).
test-cover: | $(BINDIR)
	go test -coverprofile=$(BINDIR)/coverage.out ./...
	go tool cover -func=$(BINDIR)/coverage.out

open-coverage:
	go tool cover -html=$(BINDIR)/coverage.out

clean:
	rm -rf $(BINDIR) dist
	rm -f $(NOTICE_FILE)

# ---------------------------------------------------------------------------
# Third-party license notices
# ---------------------------------------------------------------------------

notices: $(NOTICE_FILE)

$(NOTICE_FILE): go.mod go.sum
	$(call RUN,generate notices,$(GO_LICENSES) report ./cmd/poe-acp > $(NOTICE_FILE) 2>/dev/null)

check-licenses:
	$(call RUN,check licenses,$(GO_LICENSES) check ./cmd/poe-acp --disallowed_types=forbidden,restricted 2>/dev/null)

# ---------------------------------------------------------------------------
# Release / deploy
# ---------------------------------------------------------------------------

RELEASE_TAG := v$(shell cat VERSION 2>/dev/null || echo 0.0.0)

publish: build notices
	@if ! git diff --quiet -- $(NOTICE_FILE); then \
		git add $(NOTICE_FILE) && git commit -m "chore: refresh THIRD_PARTY_NOTICES.md for $(RELEASE_TAG)"; \
	fi
	@echo "Publishing $(RELEASE_TAG)..."
	git push origin main $(RELEASE_TAG)
	@echo "Pushed $(RELEASE_TAG)."

# Cross-build, detect remote OS/arch via ssh, scp the matching binary.
deploy: build-all
	@if [ -z "$(HOST)" ]; then echo "Usage: make deploy HOST=<hostname>"; exit 1; fi
	@INFO=$$(ssh -o ConnectTimeout=5 $(HOST) "uname -s -m") || { echo "Cannot reach $(HOST)"; exit 1; }; \
	OS=$$(echo "$$INFO" | awk '{print $$1}'); \
	ARCH=$$(echo "$$INFO" | awk '{print $$2}'); \
	case "$$OS-$$ARCH" in \
		Linux-aarch64|Linux-arm64)   BIN=$(BINARY)-linux-arm64 ;; \
		Linux-armv6l|Linux-armv7l)   BIN=$(BINARY)-linux-armv6 ;; \
		Linux-x86_64)                BIN=$(BINARY)-linux-amd64 ;; \
		Darwin-arm64)                BIN=$(BINARY)-darwin-arm64 ;; \
		Darwin-x86_64)               BIN=$(BINARY)-darwin-amd64 ;; \
		*) echo "Unsupported platform: $$OS $$ARCH"; exit 1 ;; \
	esac; \
	echo "Deploying to $(HOST) ($$OS/$$ARCH → $$BIN)..."; \
	scp -q $$BIN $(HOST):~/.local/bin/poe-acp && \
	ssh $(HOST) "chmod +x ~/.local/bin/poe-acp && ~/.local/bin/poe-acp --version"
