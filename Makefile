# Targets for the NFS RWX verification suite. Budgets follow Section 4.2 of
# docs/01-test-plan.md; see docs/implementation-plan.md for the delivery order.
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
all: fmt vet unit build locktool

.PHONY: build
build:
	go build ./...

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: check-fmt
check-fmt:
	@unformatted="$$(gofmt -l .)"; test -z "$$unformatted" || { echo "Unformatted files found:"; printf '%s\n' "$$unformatted"; exit 1; }

.PHONY: vet
vet:
	go vet ./...

# locktool is the byte-range lock tool the lock cases stream into an existing
# pod over pods/exec. No image and no registry: a binary this repository owns,
# gated on a registry an operator has to populate, would mean the byte-range
# cases never run anywhere, and a case that is skipped everywhere does not
# exist. See docs/05-data-path-and-locktool-design.md section 6.
#
# One binary per node architecture; the harness picks the matching one from what
# the node itself reports. CGO_ENABLED=0 makes them static, so they run on a
# busybox image as happily as on a glibc one. bin/ is git-ignored: a repository
# that ships compiled artifacts cannot be reviewed.
LOCKTOOL_ARCHES ?= amd64 arm64

.PHONY: locktool
locktool:
	@mkdir -p bin
	@for arch in $(LOCKTOOL_ARCHES); do \
		echo "building bin/locktool-linux-$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -o bin/locktool-linux-$$arch ./cmd/locktool || exit 1; \
	done

# Unit tests for the harness itself. No cluster required.
.PHONY: unit
unit:
	go test ./pkg/...

# Section 0. Budget: under 3 minutes. Nothing else runs until this passes.
.PHONY: preflight
preflight:
	go run ./cmd/preflight -timeout=3m $(COMMON)

# E2E test targets. Tests are organized strictly by category:
# PROV, DATA, CHAOS, OBS, SEC (and later SCALE, SKEW).

.PHONY: test-prov
test-prov:
	go test $(PKG) -v -timeout=45m -run '^TestProv' $(COMMON)

# test-data injects faults. DATA-12 and DATA-13 kill the NFS server process
# through pkg/chaos, because the suite sorts strictly by category and they are
# DATA cases. test-prov and test-obs are already in the same position; this is
# where the repository is rather than something the durability pair introduced,
# but a reader of this file should not have to infer it.
.PHONY: test-data
test-data:
	go test $(PKG) -v -timeout=45m -run '^TestData' $(COMMON)

.PHONY: test-chaos
test-chaos:
	go test $(PKG) -v -timeout=240m -run '^TestChaos' $(COMMON)

.PHONY: test-obs
test-obs:
	go test $(PKG) -v -timeout=45m -run '^TestObs' $(COMMON)

.PHONY: test-sec
test-sec:
	go test $(PKG) -v -timeout=30m -run '^TestSec' $(COMMON)

# Everything across all categories.
.PHONY: test-e2e
test-e2e:
	go test $(PKG) -v -timeout=300m $(COMMON)

.PHONY: clean
clean:
	rm -rf artifacts bin
