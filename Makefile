.PHONY: build build-all install test test-cover test-race vet fmt clean tidy check-licenses notices _all_parallel $(CROSS_TARGETS)

# Output directory for all build artifacts
BINDIR    := bin
BINARY    := $(BINDIR)/poe-acp
NOTICE_FILE := THIRD_PARTY_NOTICES.md

# Go binary install path
GOBIN     := $(shell go env GOPATH)/bin
VERSION   := $(shell cat VERSION 2>/dev/null || echo dev)

# Compute version metadata
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null)
GIT_TAG    := $(shell git describe --exact-match --tags HEAD 2>/dev/null)
GIT_DIRTY  := $(shell git diff --quiet 2>/dev/null || echo .dirty)
ifneq ($(GIT_TAG),v$(VERSION))
  ifneq ($(GIT_COMMIT),)
    VERSION := $(VERSION)-dev+$(GIT_COMMIT)$(GIT_DIRTY)
  endif
endif

LDFLAGS   := -s -w -X main.version=$(VERSION)

# ---------------------------------------------------------------------------
# Quiet build helpers — print a short step name, show output only on failure.
# Usage: $(call RUN,label,command)
# Set V=1 for verbose output: make all V=1
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

build: tidy
	@mkdir -p $(BINDIR)
	$(call RUN,build (native),go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/poe-acp/)

# `make all` runs fmt first, then everything else in parallel via recursive make -j.
# TIDY_DONE=1 tells sub-targets to skip redundant go-mod-tidy (already ran here).
all: fmt tidy
	@$(MAKE) -j --no-print-directory _all_parallel TIDY_DONE=1

_all_parallel: vet test-race build-all check-licenses

fmt:
	@gofmt -s -w .

install:
	go install -ldflags="$(LDFLAGS)" ./cmd/poe-acp/

# Ensure modules are tidy once; other targets depend on this.
# Skipped when TIDY_DONE=1 (set by `make all` after running tidy upfront).
tidy:
ifndef TIDY_DONE
	@go mod tidy
endif

# Cross-compile for all targets (each is an independent phony target for parallelism).
# Matches fir's target matrix: darwin/{arm64,amd64}, linux/{armv6,arm64,amd64}.
CROSS_TARGETS := build-darwin-arm64 build-darwin-amd64 build-linux-armv6 build-linux-arm64 build-linux-amd64

build-all: $(CROSS_TARGETS) build

build-darwin-arm64: | $(BINDIR)
	$(call RUN,build darwin/arm64,GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY)-darwin-arm64 ./cmd/poe-acp/)

build-darwin-amd64: | $(BINDIR)
	$(call RUN,build darwin/amd64,GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY)-darwin-amd64 ./cmd/poe-acp/)

build-linux-armv6: | $(BINDIR)
	$(call RUN,build linux/armv6,GOOS=linux GOARCH=arm GOARM=6 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY)-linux-armv6 ./cmd/poe-acp/)

build-linux-arm64: | $(BINDIR)
	$(call RUN,build linux/arm64,GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY)-linux-arm64 ./cmd/poe-acp/)

build-linux-amd64: | $(BINDIR)
	$(call RUN,build linux/amd64,GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY)-linux-amd64 ./cmd/poe-acp/)

$(BINDIR):
	@mkdir -p $(BINDIR)

test: tidy
	go test ./...

# run-tests runs the unit tests with a 100% coverage gate (paths in .covignore
# excluded). Usage: make run-tests TEST_FLAGS="-race -shuffle=on"
.PHONY: run-tests
run-tests: tidy
	@mkdir -p $(BINDIR)
	@tmpfile=$$(mktemp); \
	trap 'rm -f $$tmpfile' EXIT; \
	if ! go test -cover $(TEST_FLAGS) ./... -coverprofile=$(BINDIR)/coverage.tmp.out > $$tmpfile 2>&1; then \
		cat $$tmpfile; exit 1; \
	fi
	@go run github.com/kfet/covgate/cmd/covgate@v0.1.0 \
		-profile=$(BINDIR)/coverage.tmp.out -out=$(BINDIR)/coverage.out \
		-ignore=.covignore -min=100

test-cover: tidy
	@mkdir -p $(BINDIR)
	go test -coverprofile=$(BINDIR)/coverage.out ./...
	go tool cover -func=$(BINDIR)/coverage.out

.PHONY: open-coverage
open-coverage:
	go tool cover -html=$(BINDIR)/coverage.out

test-race: tidy
	$(call RUN,test (race),$(MAKE) --no-print-directory run-tests TEST_FLAGS="-race -shuffle=on")

vet:
	$(call RUN,vet,go vet ./...)

clean:
	rm -rf $(BINDIR)
	rm -rf dist
	rm -f $(NOTICE_FILE)

# ---------------------------------------------------------------------------
# Third-party license notices
#
# `make notices` generates THIRD_PARTY_NOTICES.md from go.mod / go.sum via
# go-licenses. `make check-licenses` fails the build if any dependency is
# under a disallowed license.
# ---------------------------------------------------------------------------

GO_LICENSES := go run github.com/google/go-licenses@v1.6.0

notices: $(NOTICE_FILE)

$(NOTICE_FILE): go.mod go.sum
	$(call RUN,generate notices,$(GO_LICENSES) report ./cmd/poe-acp > $(NOTICE_FILE) 2>/dev/null)

check-licenses:
	$(call RUN,check licenses,$(GO_LICENSES) check ./cmd/poe-acp --disallowed_types=forbidden,restricted 2>/dev/null)

# ---------------------------------------------------------------------------
# Release publishing & remote deployment
# ---------------------------------------------------------------------------

RELEASE_TAG := v$(shell cat VERSION 2>/dev/null || echo 0.0.0)

publish: build notices
	@if ! git diff --quiet -- $(NOTICE_FILE); then \
		git add $(NOTICE_FILE) && git commit -m "chore: refresh THIRD_PARTY_NOTICES.md for $(RELEASE_TAG)"; \
	fi
	@echo "Publishing $(RELEASE_TAG)..."
	git push origin main $(RELEASE_TAG)
	@echo "Pushed $(RELEASE_TAG)."

# Deploy to a remote host via scp (auto-detects OS and arch)
# Usage: make deploy HOST=myhost
deploy: build-all
	@if [ -z "$(HOST)" ]; then echo "Usage: make deploy HOST=<hostname>"; exit 1; fi
	@INFO=$$(ssh -o ConnectTimeout=5 $(HOST) "uname -s -m") || { echo "Cannot reach $(HOST)"; exit 1; }; \
	OS=$$(echo "$$INFO" | awk '{print $$1}'); \
	ARCH=$$(echo "$$INFO" | awk '{print $$2}'); \
	case "$$OS-$$ARCH" in \
		Linux-aarch64|Linux-arm64)   BIN=$(BINARY)-linux-arm64 ;; \
		Linux-armv6l)                BIN=$(BINARY)-linux-armv6 ;; \
		Linux-armv7l)                BIN=$(BINARY)-linux-armv6 ;; \
		Linux-x86_64)                BIN=$(BINARY)-linux-amd64 ;; \
		Darwin-arm64)                BIN=$(BINARY)-darwin-arm64 ;; \
		Darwin-x86_64)               BIN=$(BINARY)-darwin-amd64 ;; \
		*) echo "Unsupported platform: $$OS $$ARCH"; exit 1 ;; \
	esac; \
	echo "Deploying to $(HOST) ($$OS/$$ARCH → $$BIN)..."; \
	scp -q $$BIN $(HOST):~/.local/bin/poe-acp && \
	ssh $(HOST) "chmod +x ~/.local/bin/poe-acp && ~/.local/bin/poe-acp --version"
