# Implementation plan

Author: mikebz@
Created: 2026-09-10
Updated: 2026-09-12

**A working document, and a temporary one.** It holds the delivery order and
what is done, nothing else: no design decisions, no test approach, no
restatement of the cases. Those have homes, and a copy here is how these
documents drifted the first time. It sits in the root rather than in `docs/`, and carries no number, because it
is not part of the record those documents keep: it goes away when the last step
lands.

- What gets verified: [`docs/01-test-plan.md`](docs/01-test-plan.md).
- How the harness works, and every flag: the repository
  [`README.md`](README.md).
- Why a phase is built the way it is: that phase's design doc, listed in the
  [`README.md`](README.md).
- What a real run taught: [`docs/findings.md`](docs/findings.md).

Rule for the whole effort: **small, reviewable changes.** One vector per change,
each landing with the harness pieces it needs and nothing more. A change that
adds a helper no case calls yet does not land. A change estimated at 1000 lines
or more lands as a design doc first ([`AGENTS.md`](AGENTS.md)).

## Delivery order

One step is one pull request, or a short run of them. Later steps depend only on
earlier ones. Steps are numbered independently of the documents; a step's design
doc, where it has one, is named in its row.

| Step | Scope | Status |
|---|---|---|
| 1 | Approach, harness skeleton, preflight (Section 0), PROV-01, DATA-03, DATA-05 `flock` | done, [PR #1](https://github.com/mikebz/nfs-verification/pull/1) |
| 2 | Cases needing nothing new from the harness: PROV-03, PROV-04, DATA-01, DATA-04, SEC-01 | done, [PR #3](https://github.com/mikebz/nfs-verification/pull/3) |
| 2b | The three held back from step 2: DATA-02, OBS-04, SEC-02 | done, [PR #4](https://github.com/mikebz/nfs-verification/pull/4) |
| 3 | `pkg/chaos`, CHAOS-01, CHAOS-02, the SLO measurement path, fault timelines. [Design](docs/03-chaos-operations-design.md) | done, [PR #4](https://github.com/mikebz/nfs-verification/pull/4) |
| 4 | Grace and lock reclaim: CHAOS-05, CHAOS-06, CHAOS-07, OBS-02, OBS-03. [Design](docs/04-grace-and-lock-reclaim-design.md) | done, [PR #8](https://github.com/mikebz/nfs-verification/pull/8) |
| 5 | Close out PROV: PROV-02, PROV-05 to PROV-11 | done, [PR #10](https://github.com/mikebz/nfs-verification/pull/10) |
| 6 | Close out DATA: DATA-06 to DATA-13, `locktool`. [Design](docs/05-data-path-and-locktool-design.md) | done except the soak, [PR #12](https://github.com/mikebz/nfs-verification/pull/12) onward. DATA-10 has not been run; DATA-14 is deferred (test plan Section 3.2) |
| 7 | OBS: OBS-05, OBS-06, OBS-07 and the half of OBS-01 that needs no fault. [Design](docs/06-observability-design.md) | **in progress**, first of three PRs done ([PR #29](https://github.com/mikebz/nfs-verification/pull/29)): the kubelet stats reader and OBS-06, red on the quota check ([F-009](docs/findings.md)). Section 3.5 stays open either way: OBS-01's behavioral half needs a fault from step 10 |
| 8 | Close out SEC: SEC-03 to SEC-09 | not started |
| 9 | Close out SCALE: SCALE-01 to SCALE-07 | not started |
| 10 | Close out CHAOS: CHAOS-03, CHAOS-04, CHAOS-08 to CHAOS-18, and OBS-01's behavioral half | not started |
| 11 | SKEW-01 to SKEW-03, conditional on preflight finding independent versioning | not started |

Thirty-five cases are in the tree today. The repository
[`README.md`](README.md) lists them and says what each asserts.

## Why this order

Steps 1 to 4 built the foundation: the harness, the eleven cases that need no
fault, the fault-injection package, and the grace and lock reclaim path.

From step 5 on, **one section of the test plan is closed out at a time** rather
than interleaving fault injection across domains. PROV first, because the
lifecycle has to be trustworthy before anything built on it means much; DATA
next, because the data path is what the protocol actually guarantees; then OBS
and SEC; then SCALE; then CHAOS last, because the most invasive
platform-dependent faults are worth running only once a failure elsewhere can be
ruled out; then SKEW, if preflight finds the server and driver independently
versioned.
