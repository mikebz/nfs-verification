# Targets for the NFS RWX verification suite. Budgets follow Section 4.2 of
# docs/01-test-plan.md; see docs/plan.md for the delivery order.
#
# Every target takes FLAGS for cluster-specific values, for example:
#
#   make test-e2e FLAGS="-storage-class=nfs -lease-seconds=60 -grace-seconds=90"
#
SHELL := /bin/bash
PKG := ./test/e2e
RUN_ID ?= $(shell date -u +%Y%m%d-%H%M%S)
FLAGS ?=
COMMON := -run-id=$(RUN_ID) $(FLAGS)

.PHONY: all
all: fmt vet unit build

.PHONY: build
build:
	go build ./...

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: check-fmt
check-fmt:
	@test -z "$$(gofmt -l .)" || (echo "Unformatted files found:" && gofmt -l . && exit 1)

.PHONY: vet
vet:
	go vet ./...

# Unit tests for the harness itself. No cluster required.
.PHONY: unit
unit:
	go test ./pkg/...

# Section 0. Budget: under 3 minutes. Nothing else runs until this passes.
.PHONY: preflight
preflight:
	go run ./cmd/preflight -timeout=3m $(COMMON)

# The end-to-end cases. Budget: under 15 minutes on 2 nodes. Select a subset
# with go test -run when there is more than one category of case to select.
.PHONY: test-e2e
test-e2e:
	go test $(PKG) -v -timeout=30m $(COMMON)

.PHONY: clean
clean:
	rm -rf artifacts
