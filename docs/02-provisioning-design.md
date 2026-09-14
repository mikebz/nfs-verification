# 02: Provisioning and volume lifecycle design

Author: mikebz@
Created: 2026-09-14
Updated: 2026-09-14
Status: shipped, delivery steps 1 ([PR #1](https://github.com/mikebz/nfs-verification/pull/1)),
2 ([PR #3](https://github.com/mikebz/nfs-verification/pull/3)),
and 5 ([PR #10](https://github.com/mikebz/nfs-verification/pull/10)).
PROV-01 through PROV-11 shipped.
Serves: PROV-01 through PROV-11 (complete Provisioning and Volume Lifecycle test group).
Requirements in [`01-test-plan.md`](01-test-plan.md) Section 3.1.
Builds on [`01-test-plan.md`](01-test-plan.md).

---

## 1. Scope and guarantees under test

The provisioning test group verifies that the storage provisioner and CSI driver faithfully
implement the Kubernetes Persistent Volume lifecycle for NFS ReadWriteMany (RWX) volumes.
Every test runs against a real Kubernetes cluster and asserts client-observable behavior through
standard Kubernetes API objects, CSI controller interactions, and pod-level filesystem operations.

NFS RWX volumes present unique lifecycle challenges that do not exist for single-node (RWO) block storage:
1. **Dynamic provisioning and multi-node attachment**: When a claim is created, the provisioner
   must mint an export, allocate backing storage, configure server-side export permissions, and
   bind the volume. Because the volume is RWX, multiple pods on distinct worker nodes must mount
   the identical export simultaneously and perform concurrent I/O without cross-contamination (`PROV-01`).
2. **Safe deletion and unmount ordering**: A `hard` NFSv4.1 mount blocks indefinitely if its backing
   export is destroyed while still mounted. Kubernetes Storage Object in Use Protection
   (`kubernetes.io/pvc-protection`) must prevent claim deletion while pods mount the volume (`PROV-03`),
   and teardown must never delete a claim until all mounting pods have departed the API ([F-001](findings.md)).
3. **Reclaim policies (`Delete` vs `Retain`)**: A claim with reclaim policy `Delete` must remove the
   underlying PV and wipe backing storage to prevent capacity leaks (`PROV-01`). A claim with `Retain`
   must preserve the PV in `Released` phase upon claim deletion, allowing manual clearance of `claimRef`,
   successful re-binding to a new claim, and verified data preservation (`PROV-06`).
4. **Volume expansion (`PROV-04`, `PROV-11`)**: CSI drivers advertising `EXPAND_VOLUME` must grow
   the backing volume and export capacity without data corruption, client restarts, or unmounts. If the
   StorageClass does not support expansion, the Kubernetes API must reject the resize cleanly rather
   than leaving the claim permanently wedged in `Resizing`.
5. **Snapshots and restoration (`PROV-05`)**: When advertised by a `VolumeSnapshotClass` targeting the
   CSI driver, snapshots must capture volume state consistently, reach `readyToUse`, and restore to a
   working RWX volume matching the source data. When unsupported, requests must either be rejected outright
   or remain unready, never falsely reporting `readyToUse`.
6. **Resilience during control plane outages (`PROV-07`, `PROV-08`)**: Creating or deleting claims
   while the NFS server pod is down must not cause orphaned exports, permanent API deadlocks, or leaked
   storage once the server recovers.
7. **Concurrency, churn, and boundary naming (`PROV-02`, `PROV-09`, `PROV-10`)**: Rapid creation and
   deletion cycles (100 cycles) must not leak file descriptors or export IDs; 20 concurrent claims must
   receive unique export paths and IDs; and maximum-length RFC 1123 resource names (253 characters)
   must produce valid, uncorrupted export configurations.

---

## 2. Harness architecture and mechanisms

The harness drives volume lifecycle tests through standard Kubernetes client interfaces and hermetic
fixtures:

- **Volume binding mode handling (`pkg/framework/pvc.go`)**:
  StorageClasses configure either `VolumeBindingImmediate` or `VolumeBindingWaitForFirstConsumer`.
  Under `WaitForFirstConsumer`, an unbound claim never reaches `Bound` phase until a pod requesting it is
  scheduled onto a node ([F-002](findings.md)). The harness inspects the StorageClass binding mode and
  automatically schedules consumer pods prior to awaiting the `Bound` condition.
- **Teardown ordering and claim retention (`pkg/framework/framework.go`)**:
  To protect worker nodes from catastrophic NFS wedging, harness teardown (`DeleteCaseObjects`) enforces a strict invariant:
  pods are deleted gracefully first and awaited until completely gone from the API. Only then are claims
  deleted. If an unmount was never observed, the harness marks the claim unproven and preserves it on
  the cluster rather than risking node wedging ([F-001](findings.md), [F-003](findings.md)).
- **CSI capability gating**:
  Expansion and snapshot capabilities are optional under the CSI specification. Preflight discovers
  whether the StorageClass advertises `allowVolumeExpansion` and whether `VolumeSnapshotClasses` target
  the active CSI driver. Cases test clean rejection or persistent unready state when capabilities are absent and assert end-to-end
  workflows when they are present.
- **Capacity measurement and quota detection (`pkg/framework/io.go`, `pkg/framework/probes.go`)**:
  Pod filesystem space is evaluated via `MountCapacity` in `pkg/framework/io.go` (driving `stat -f` with a `df` fallback)
  and `MountSpace` in `pkg/framework/probes.go` (reading byte and inode headroom). When evaluating expansion, the harness
  distinguishes whether capacity growth is visible to client `df`/`stat -f` (enforced per-volume quota) or whether
  the export shares a backing filesystem without per-volume quota enforcement ([F-009](findings.md)).
- **Fault-coordinated provisioning (`pkg/chaos`)**:
  Server process and pod outage cases (`PROV-07`, `PROV-08`) coordinate directly with `pkg/chaos` to
  terminate server pods while observing PVC controller transition loops and replacement pod readiness.

---

## 3. Safety invariants and cluster protection

Because incorrect teardown or premature deletion of an NFS export can take worker nodes out of service,
the provisioning test group adheres to strict architectural safety rules:

1. **Unmount before deletion (F-001)**: Never delete a claim a surviving pod still mounts. Force-deleting
   pods or deleting PVCs out of order causes kernel NFS client threads to hang in uninterruptible sleep (`D` state),
   eventually wedging kubelet volume manager reconcilers and requiring a physical node reboot.
2. **Sustained PVC protection observation**: In `PROV-03`, the harness asserts that the deleted PVC
   carries `kubernetes.io/pvc-protection` and remains in `Terminating` phase across a sustained 20-second
   polling window, rather than checking a single instantaneous API snapshot.
3. **Reclaim policy restoration on success**: `PROV-06` mutates the PV reclaim policy to `Retain` to assert
   PV preservation across claim deletion, and restores it to `Delete` at the conclusion of the test run to allow
   automated framework teardown to reclaim backing storage (a mid-test failure before this point leaves the PV in `Retain` for manual triage).
4. **Capability-guarded skips**: Tests requiring multi-node scheduling (`PROV-01`, `PROV-05`, `PROV-10`)
   guard execution via `requireCap(t, f.Caps.MultiNode, ...)`. A single-worker cluster skips cleanly
   rather than failing.

---

## 4. Provisioning cases summary

Detailed requirements and profile-driven timing live in [`01-test-plan.md`](01-test-plan.md) Section 3.1.

| Case | Status | Expected result | Citation / Basis |
|---|---|---|---|
| **PROV-01** | Shipped | Dynamic provision RWX claim, bind, mount on 2 nodes, write/read checksum, delete claim and verify PV removed | Kubernetes PV lifecycle, RFC 8881 Sec 10 |
| **PROV-02** | Shipped | 20 RWX claims provisioned concurrently: all bind, unique export IDs and paths, zero server restarts | NFS server export concurrency |
| **PROV-03** | Shipped | Delete PVC while pod still mounts: PVC stays Terminating with pvc-protection finalizer; I/O continues; deletes after pod departure | Kubernetes Storage Object In Use Protection, [F-001](findings.md) |
| **PROV-04** | Shipped | Volume expansion: if supported, capacity grows, data intact, zero client restarts; if unsupported, API rejects cleanly | Kubernetes Volume Expansion, CSI `EXPAND_VOLUME` |
| **PROV-05** | Shipped | Snapshot and restore: restored volume mounts RWX across two nodes, content matches source; if unsupported, clean rejection or stays unready | CSI `CREATE_DELETE_SNAPSHOT`, VolumeSnapshot API |
| **PROV-06** | Shipped | Reclaim policy Retain: PV survives claim deletion in Released phase; after claimRef cleared, rebinds to new PVC with data intact | Kubernetes Reclaim Policies |
| **PROV-07** | Shipped | Provision PVC while server pod is down: claim stays Pending during outage; binds and mounts after recovery with zero orphaned exports | Storage control plane resilience, Archetype A gateway |
| **PROV-08** | Shipped | Delete PVC while server pod is down (pod unmounted first): PVC and PV complete deletion after server recovery without storage leaks | CSI controller unpublish/delete retry, [F-001](findings.md) |
| **PROV-09** | Shipped | Rapid create/delete churn (100 cycles): zero export ID exhaustion, zero fd leaks, zero server restarts | Provisioner state machine stability under churn |
| **PROV-10** | Shipped | Volume name edge cases: admission rejects 1000-char and uppercase names; 253-char boundary name provisions, binds, mounts, and passes cross-node I/O | RFC 1123 DNS subdomain syntax, export configuration parser |
| **PROV-11** | Shipped | Two-stage expansion under active I/O: background write load runs without errors; capacity expands; all committed writes survive | CSI Online Expansion, active workload integrity |

---

## 5. Detailed case walkthroughs

### PROV-01: Dynamic provisioning, multi-node mount, and reclaim deletion
- **Steps**:
  1. Create an RWX claim on the active StorageClass.
  2. Spawn a writer pod on Node A and a reader pod on Node B mounting the claim.
  3. Wait for the claim to reach `Bound` phase (accounting for `WaitForFirstConsumer` scheduling).
  4. Write a 1MiB pseudorandom payload from writer on Node A and close descriptor.
  5. Compute SHA-256 checksum from reader on Node B across the network, asserting payload integrity.
  6. Delete both pods gracefully and await their departure from the Kubernetes API.
  7. Delete the claim and wait for it to be removed.
  8. If PV reclaim policy is `Delete`, wait for backing PV to be completely removed from the cluster.
     If `Retain`, skip PV removal check (`PROV-06` covers retention).

### PROV-02: Concurrent provisioning of 20 RWX claims
- **Steps**:
  1. Record baseline server container restart count.
  2. Launch 20 concurrent goroutines, each creating an RWX PVC with a unique name.
  3. If StorageClass uses `WaitForFirstConsumer`, spawn a consumer pod for each claim.
  4. Wait for all 20 claims to reach `Bound` phase within `BindTimeout`.
  5. Inspect backing PV for each claim and assert:
     a. Every PV has a unique name.
     b. Every PV with an `Export_Id` annotation has a unique ID across the set.
     c. Every PV has a unique export path or CSI `volumeHandle`.
  6. Assert server container restart count did not increase during concurrent provisioning.

### PROV-03: Storage Object in Use Protection under active mount
- **Steps**:
  1. Provision an RWX claim, mount in a pod, and write a 1MiB test file.
  2. Issue a deletion request against the PVC while the pod still mounts it.
  3. Poll for 20 seconds: assert the PVC remains present in `Terminating` status and actively carries the
     `kubernetes.io/pvc-protection` finalizer.
  4. Perform reads and writes through the pod's mount while the claim is `Terminating`, asserting that the
     underlying export remains fully functional.
  5. Delete the pod gracefully and wait for it to leave the API.
  6. Assert the PVC completes deletion and disappears from the API once the mount is gone.

### PROV-04: Volume expansion with and without driver support
- **Steps**:
  1. Provision an RWX claim, mount it, and read initial storage request.
  2. If StorageClass lacks expansion support (`CanExpand == false`):
     a. Request 1GiB expansion and assert the Kubernetes API rejects the modification with an error.
     b. Re-read claim and assert requested capacity remained unchanged, preventing wedged `Resizing` claims.
  3. If StorageClass supports expansion (`CanExpand == true`):
     a. Write a 1MiB payload and record its SHA-256 checksum.
     b. Record initial client `df` capacity and pod restart count.
     c. Request 1GiB expansion and wait for `status.capacity` to reach the requested size.
     d. Assert data written before expansion remains readable and intact.
     e. Assert client pod did not restart and share remains writable.
     f. Verify client `df` output: growth is logged; unmoving `df` is logged with export quota explanation ([F-009](findings.md)); capacity drop fails.

### PROV-05: VolumeSnapshot creation, readiness, and restoration
- **Steps**:
  1. Provision an RWX claim, mount in a pod, write a known payload with SHA-256 checksum, then unmount pod.
  2. Inspect cluster for `VolumeSnapshot` CRD. If missing, assert claim with snapshot `dataSource` is rejected.
  3. Inspect `VolumeSnapshotClasses`. If none targets the active CSI driver, assert snapshot creation is rejected
     or stays unready, never falsely reporting `readyToUse`.
  4. If advertised and supported:
     a. Create a `VolumeSnapshot` pointing to the source claim.
     b. Wait for `VolumeSnapshot` status to become `readyToUse: true`.
     c. Create a new claim with `dataSource` referencing the `VolumeSnapshot`.
     d. Wait for restored claim to bind and assert its access mode is `ReadWriteMany`.
     e. Mount restored claim in pods on two nodes; verify data matches source checksum on both.
  5. Clean up restored pods, claims, and snapshot.

### PROV-06: Reclaim policy Retain, preservation, and re-binding
- **Steps**:
  1. Provision an RWX claim, mount in a pod, and write a 1MiB payload with known checksum.
  2. Resolve underlying PV and update its `persistentVolumeReclaimPolicy` to `Retain`.
  3. Delete the pod gracefully and wait for it to leave the API.
  4. Delete the PVC and wait for it to disappear from the API.
  5. Assert the PV persists in `Released` phase rather than being deleted.
  6. Clear the PV's `claimRef` to transition its phase from `Released` to `Available`.
  7. Create a new PVC explicitly referencing the retained PV by `volumeName`.
  8. Wait for the new PVC to bind to the retained PV.
  9. Mount the rebound claim in a new pod and verify SHA-256 checksum matches original written data.
  10. Restore the PV reclaim policy to `Delete` so teardown cleanly reclaims backing storage.

### PROV-07: Provisioning during server pod outage
- **Steps**:
  1. Identify target NFS server pod and controller. Skip if unmanaged.
  2. Terminate server pod using `pkg/chaos`.
  3. Create an RWX PVC while the server pod is offline.
  4. If `WaitForFirstConsumer`, schedule a consumer pod without awaiting readiness.
  5. Poll throughout server outage: assert PVC remains in `Pending` phase and does not prematurely bind.
  6. Wait for replacement server pod to become `Ready`.
  7. Wait for PVC to bind and consumer pod to become `Ready`.
  8. Write test payload to share and verify checksum on read-back.
  9. Delete pod and claim, verifying no orphaned exports remain after recovery.

### PROV-08: PVC deletion during server pod outage
- **Steps**:
  1. Provision an RWX claim, mount in a pod, and write a test file.
  2. Gracefully delete consumer pod and await its API departure before touching the server (preventing [F-001](findings.md)).
  3. Identify server pod and delete it via `pkg/chaos`.
  4. Delete the PVC while the server pod is offline.
  5. Wait for replacement server pod to recover and become `Ready`.
  6. Wait for the PVC to finish deleting and leave the API.
  7. If reclaim policy was `Delete`, verify the backing PV is completely removed without storage leaks.

### PROV-09: Rapid create/delete churn (100 cycles)
- **Steps**:
  1. Skip if running in `-short` mode.
  2. Record baseline server container restart count.
  3. Execute 100 sequential cycles: create PVC -> await `Bound` -> delete PVC -> await removal.
  4. Assert all 100 cycles complete successfully without timeouts or export allocation errors.
  5. Assert server container restart count did not increase (verifying absence of server memory crashes or leaks).

### PROV-10: Volume name boundary cases (253-character RFC 1123 limit)
- **Steps**:
  1. Verify admission barriers: attempt creating PVC with 1000-character name; assert apiserver rejection with `Invalid` or `BadRequest`.
  2. Verify character set barriers: attempt creating PVC with uppercase characters; assert apiserver rejection.
  3. Construct maximum-length valid RFC 1123 name (exactly 253 characters) incorporating framework run prefix.
  4. Create claim with the 253-character name and mount across two nodes.
  5. Wait for claim to bind. Resolve PV and inspect CSI `volumeHandle`, export server, and export path:
     - Assert neither server nor path contains newline characters (`\r` or `\n`).
     - Report `blocked` if export configuration cannot be extracted (preventing false passes without export inspection).
  6. Write 1MiB payload from Node A and verify SHA-256 checksum from Node B.
  7. Gracefully delete pods and claim. Assert server remained healthy with zero restarts.

### PROV-11: Two-stage expansion under active write load
- **Steps**:
  1. Provision an RWX claim and mount in a pod.
  2. If expansion is unsupported, assert clean rejection as in `PROV-04`.
  3. If expansion is supported:
     a. Record initial `df` capacity, pod restarts, and server restarts.
     b. Start background write load generating active traffic (`StartWriteLoad`).
     c. Request 1GiB expansion while write load is actively executing.
     d. Wait for claim `status.capacity` to reflect new size.
     e. Stop write load and parse workload report.
     f. Assert zero I/O errors (`report.Errors() == 0`) across expansion.
     g. Assert all writes acknowledged before or during expansion survive and are readable.
     h. Assert client pod and server container restart counts did not increase.
     i. Verify client `df` output before and after expansion.

---

## 6. Key decisions and invariants

- **Pod-first scheduling for WaitForFirstConsumer**: Under `volumeBindingMode: WaitForFirstConsumer`,
  the Kubernetes PV controller does not bind claims until pod scheduling constraints are evaluated.
  The harness always pairs claim creation with consumer pod creation before checking bind status ([F-002](findings.md)).
- **Unmount confirmation precedes deletion**: Storage Object in Use Protection prevents premature API
  deletion, but client kernel threads hang if the export disappears while mounted. Teardown waits for
  pod API departure and unmount confirmation before issuing claim deletion ([F-001](findings.md)).
- **Rejection or unready state is an assertion, not a skip**: When a StorageClass lacks expansion or snapshot capabilities,
  the harness does not skip the test; it actively asserts that the control plane either rejects the request cleanly (expansion)
  or leaves the object unready without falsely reporting `readyToUse` (snapshots), ensuring claims do not enter unrecoverable error phases.
- **Fault cases live in category suites**: Fault injection during provisioning (`PROV-07`, `PROV-08`)
  is categorized under `TestProv` rather than `TestChaos` to preserve functional cohesion.

---

## 7. What real runs taught

- **[F-001](findings.md) (Unmount before PVC deletion)**: Two ordinary API calls in the wrong order
  (deleting a claim while a pod still mounts it) cause uninterruptible sleep on client nodes under `hard` NFSv4.1 mounts.
- **[F-002](findings.md) (WaitForFirstConsumer claims stay Pending)**: A claim using `WaitForFirstConsumer`
  remains `Pending` indefinitely unless a consuming pod is created to drive node placement.
- **[F-003](findings.md) (Retain unproven claims in teardown)**: When unmount cannot be confirmed,
  teardown retains the PVC and PV rather than forcing deletion and wedging client nodes.
- **[F-004](findings.md) (StorageClass advertises expansion that driver cannot perform)**: In-cluster
  `nfs-server-provisioner` advertises `allowVolumeExpansion: true`, but attempts to expand claims fail or wedge.
  `PROV-04` and `PROV-11` report failures against this deployment as documented findings.
- **[F-009](findings.md) (Export has no per-volume quota)**: Directory-backed NFS exports without quota
  enforcement report the entire backing filesystem capacity via `statvfs`. Client `df` reflects the underlying disk,
  so control-plane claim expansion does not change the bytes reported by `df`.
- **[F-015](findings.md) (Bound timeouts on slow provisioners)**: Provisioning latency varies across cloud
  environments and provisioners. Timeouts are configured centrally in `pkg/framework` constants rather than literals.

---

## 8. What changed after this was written

- **Consolidation by test group**: Provisioning design documentation was consolidated from early delivery
  notes (Steps 1, 2, 5) into this single authoritative reference covering `PROV-01` through `PROV-11`.
- **Real run baseline (2026-09-13)**: On a 3-worker GKE cluster (`gke-w1`) running Kubernetes v1.37 with
  `nfs-server-provisioner` v4.0.8, nine of the eleven cases passed cleanly. `PROV-04` and `PROV-11` failed
  because the provisioner advertises expansion without implementing it ([F-004](findings.md)). These failures
  accurately reflect deployment limitations and are not test defects.

---

## 9. Sources

- Kubernetes [Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/):
  Reclaim policies, [Storage Object in Use Protection](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#storage-object-in-use-protection),
  and [Volume Expansion](https://kubernetes.io/docs/concepts/storage/persistent-volumes/#expanding-persistent-volumes-claims).
- Kubernetes [Storage Classes](https://kubernetes.io/docs/concepts/storage/storage-classes/):
  `volumeBindingMode: WaitForFirstConsumer` and `allowVolumeExpansion`.
- Kubernetes [Volume Snapshots](https://kubernetes.io/docs/concepts/storage/volume-snapshots/).
- Kubernetes [Object Names and IDs](https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#dns-subdomain-names) (RFC 1123).
- Container Storage Interface (CSI) Specification:
  `ControllerServiceCapability` (`EXPAND_VOLUME`, `CREATE_DELETE_SNAPSHOT`).
- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html), NFSv4.1:
  Section 10 (Client-Side Caching).
