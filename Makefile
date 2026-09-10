# Test gates for the NFS RWX verification suite. Budgets and contents follow
# Section 4.2 of doc/01-test-plan.md. Targets for the later gates arrive with the
# cases that fill them; see plan.md for the delivery order.
#
# Every target takes FLAGS for cluster-specific values, for example:
#
#   make test-presubmit FLAGS="-storage-class=nfs-rwx -server-namespace=nfs"
#
SHELL := /bin/bash
GO ?= go
PKG := ./test/e2e
RUN_ID ?= $(shell date -u +%Y%m%d-%H%M%S)
FLAGS ?=
COMMON := -run-id=$(RUN_ID) $(FLAGS)

.PHONY: all
all: fmt vet unit build

.PHONY: build
build:
	$(GO) build ./...

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: vet
vet:
	$(GO) vet ./...

# Unit tests for the harness itself. No cluster required.
.PHONY: unit
unit:
	$(GO) test ./pkg/...

# Section 0. Budget: under 3 minutes. Nothing else runs until this passes.
.PHONY: preflight
preflight:
	$(GO) run ./cmd/preflight -timeout=3m $(COMMON)

# All P cases. Budget: under 15 minutes on 2 nodes. Deliberately no chaos:
# chaos is slow and its failures need human triage, and putting it in the fast
# gate trains people to ignore red.
.PHONY: test-presubmit
test-presubmit:
	$(GO) test $(PKG) -v -timeout=30m -gate=presubmit $(COMMON)

.PHONY: clean
clean:
	rm -rf artifacts
