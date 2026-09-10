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
	@unformatted="$$(gofmt -l .)"; test -z "$$unformatted" || { echo "Unformatted files found:"; printf '%s\n' "$$unformatted"; exit 1; }

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

# Section 4.2 gates. Categories live in a go test pattern, not in harness
# machinery: cases that injure the server are named TestChaos..., everything
# else is presubmit. The prefix marks what a case does rather than where it sits
# in the plan, so the OBS cases that inject a fault carry it too.
#
# Presubmit deliberately holds no chaos. Chaos is slow and its failures need
# human triage; putting it in the fast gate trains people to ignore red.

# All P cases. Budget: under 15 minutes on 2 nodes.
.PHONY: test-presubmit
test-presubmit:
	go test $(PKG) -v -timeout=30m -skip '^TestChaos' $(COMMON)

# The chaos vector. Budget: under 4 hours per Section 4.2, and every case
# injures the server. Repeated failover alone is five outages end to end.
.PHONY: test-chaos
test-chaos:
	go test $(PKG) -v -timeout=240m -run '^TestChaos' $(COMMON)

# Everything. Same budget as chaos, since chaos dominates it.
.PHONY: test-e2e
test-e2e:
	go test $(PKG) -v -timeout=300m $(COMMON)

.PHONY: clean
clean:
	rm -rf artifacts
