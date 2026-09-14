# 05: Data path: consistency, concurrency, byte-range locking, and durability

Author: mikebz@
Created: 2026-09-11
Updated: 2026-09-14
Status: shipped, delivery steps 1 ([PR #1](https://github.com/mikebz/nfs-verification/pull/1)),
2 ([PR #3](https://github.com/mikebz/nfs-verification/pull/3)),
2b ([PR #4](https://github.com/mikebz/nfs-verification/pull/4)),
and 6 ([PR #12](https://github.com/mikebz/nfs-verification/pull/12) onward).
DATA-01 through DATA-13 shipped; DATA-14 deferred.
Serves: DATA-01 through DATA-14 (complete Data Path test group). Requirements in
[`01-test-plan.md`](01-test-plan.md) Section 3.2.
Builds on [`01-test-plan.md`](01-test-plan.md), [`03-chaos-operations-design.md`](03-chaos-operations-design.md),
and [`06-observability-design.md`](06-observability-design.md).

---

## 1. Why this domain exists

The data path is the primary contract an application has with persistent storage:
reading back what was written, coordinating access across concurrent clients, and
surviving unexpected crashes without data corruption.

NFS RWX volumes on Kubernetes present unique data path challenges because multiple
pods on separate worker nodes mount the exact same export simultaneously. Under
POSIX, applications expect strict read-after-write consistency and atomic appends.
NFSv4.1 makes a different, strictly bounded set of guarantees:

1. **Close-to-open cache consistency (RFC 8881 Section 10)**: Writes from pod A are
   guaranteed to be visible to pod B on another node only after pod A closes the file
   and pod B subsequently opens it (`DATA-03`). While pod A holds the file open, pod B
   may see nothing or a partial prefix (`DATA-04`), unless attribute caching is
   disabled (`DATA-08`, `noac`). Direct I/O separately bypasses page caching on both
   sides for block-aligned I/O (`DATA-07`, `O_DIRECT`).
2. **Concurrent writers without corruption**: Multiple clients writing to distinct
   files on the same share must never cross-contaminate each other (`DATA-01`).
3. **Append semantics and implementation limits (`O_APPEND`)**: NFSv4.1 has no native
   append wire operation. Clients implement `O_APPEND` by querying the end of file (EOF)
   and issuing a write at that offset. Under concurrent appends from multiple nodes,
   record ordering and exact line counts are an implementation property rather than a
   protocol guarantee (`DATA-02`). Torn records, however, represent corruption under any reading.
4. **Native byte-range locking (RFC 8881 Section 9)**: File locking in NFSv4.1 is native
   to the protocol. Clients coordinate access using advisory whole-file locks (`flock`)
   and byte-range locks (`fcntl`, `DATA-05`). Lock release upon pod force-deletion is
   strictly bounded by client lease duration (`DATA-06`, `client-identifier`).
5. **Namespace and open file handling**: When an open file is unlinked on the same node,
   the Linux client implements silly rename (`.nfs*`), preserving the open descriptor;
   when unlinked from another node, operations may receive `ESTALE` (`DATA-09`).
6. **Directory concurrency under load**: Large directory listings racing simultaneous
   deletions must complete without server crashes or use-after-free corruption (`DATA-10`).
7. **Durability across server crashes (RFC 8881 Section 18.3)**: Data committed to stable
   storage via `fsync` (`COMMIT`) must survive a server crash completely intact (`DATA-12`).
   Uncommitted unstable writes may lawfully be lost, but may never surface as corrupted
   bytes (`DATA-13`).

Done means thirteen repeatable test cases verifying every data path property client pods
can observe, supported by specialized in-pod tooling (`locktool`), node agent inspection,
and four-verdict durability sweeps.

## 2. Verification tools, readers, and fixtures

Testing data path semantics requires tools that can express subtle kernel and protocol
primitives inside minimal container environments:

- **`locktool` (`cmd/locktool`, `pkg/framework/locktool.go`)**:
  A static Go binary (~200 lines) built by `make locktool` for both `linux/amd64` and
  `linux/arm64`. It is streamed into client pods over `pods/exec` and verified via
  SHA-256 checksum. It requires no container image modification and no external registry.
  - `hold`: Acquires a byte range (`F_SETLK`) and keeps the file descriptor open without
    blocking indefinitely in `F_SETLKW` (polling to avoid unkillable `D`-state hangs
    on hard mounts).
  - `try`: Probes acquisition of a range (`F_SETLK`), reporting success or conflict offset/length.
  - `getlk`: Queries the server's lock table via `F_GETLK` without acquiring, proving whether
    an existing client still holds its allocated range.
- **Node agent `/proc/locks` reader (`pkg/framework/locks.go`)**:
  Reads the client node's own `/proc/locks` table via the privileged node agent.
  Matches locks using node-specific anonymous device numbers (`st_dev`) and inodes,
  distinguishing a client's belief about a lock from what the server reports ([F-010](findings.md)).
- **Clone volume mechanism (`pkg/framework/nfssource.go`)**:
  Mount options belong to the volume in Kubernetes. To test attribute caching (`DATA-08`, `noac`),
  the harness inspects the dynamically provisioned claim's PV, extracts its server and path,
  and provisions a static clone PV pointing to the identical export with `noac` applied.
- **Content-verifying record sweep (`pkg/framework/sweep.go`)**:
  Replaces naive existence checks with a single-pass `pods/exec` verification that checks
  record indices, lengths, and SHA-256 checksums, returning four exhaustive verdicts:
  `correct`, `absent`, `short`, and `wrong` ([F-007](findings.md)).
- **Directory census helper (`pkg/framework/dircensus.go`)**:
  Streams directory entries using `find -mindepth 1 -maxdepth 1` rather than `ls` (which sorts
  in-memory and triggers OOM on 100k entries). Validates that all returned names were legitimately
  created.
- **Kernel ring buffer monitor (`dmesg`)**:
  Captures kernel logs on client nodes before and after tests to detect memory corruptions,
  KASAN splats, or filesystem use-after-free bugs that do not immediately crash userspace.

## 3. What a byte-range lock is

File locking in NFSv4.1 rests on principles that differ fundamentally from local filesystems:

1. **The protocol locks ranges natively**: RFC 8881 Section 9 defines `LOCK`, `LOCKT`,
   and `LOCKU`, each operating on an offset and length. Unlike NFSv3 (which relied on the
   external NLM sidecar protocol), locking is built directly into the NFSv4.1 state model.
2. **The Linux client translates whole-file locks to ranges**: The Linux kernel client
   (`fs/nfs/file.c`) simulates `flock(2)` using POSIX byte-range locks over the entire file
   (`[0, EOF)`).
3. **Four application calls, one wire operation**:

   | Application Call | `/proc/locks` Type | Byte Range | Released When |
   |---|---|---|---|
   | `flock(2)` | `FLOCK` | Whole file (`0..EOF`) | Last descriptor of open file description closes |
   | `fcntl(F_SETLK/F_SETLKW)` | `POSIX` | Yes (`start..len`) | **Any** descriptor to that file is closed by process, or exit |
   | `fcntl(F_OFD_SETLK)` | `OFDLCK` | Yes (`start..len`) | The specific descriptor that acquired it closes |
   | `fcntl(F_GETLK)` | Query | Yes (`start..len`) | N/A (query only) |

4. **One lease per client per server**: Under the Linux NFS client architecture
   ([kernel client-identifier](https://docs.kernel.org/filesystems/nfs/client-identifier.html)),
   all mounts and pods on a single Kubernetes worker node share a single client lease with the NFS server:
   - When a pod dies gracefully or its process terminates, the kernel closes file descriptors,
     releasing locks within ~1 second (`DATA-06`).
   - When an entire node dies, the server retains its locks until the client lease expires (`lease_time`).
5. **Mount options that disable server locking**: Mount options like `nolock` and
   `local_lock=all|flock|posix` keep locks local to the client node (`fs/nfs/fs_context.c`).
   On such mounts, cross-node locking does not exist. Test cases read `/proc/mounts` first
   and report `blocked` rather than falsely failing the storage system.

## 4. What these cases assert

Conventions shared across the suite (multi-node capability guards, bounded timeouts,
profile-driven timing) live in [`01-test-plan.md`](01-test-plan.md) Section 4.1.

| Case | Status | Assertion | Source & Basis |
|---|---|---|---|
| **DATA-01** | Shipped | 4 pods concurrently write 1MiB files: all checksums match, no cross-contamination, exactly 4 directory entries | Multi-writer file partitioning, RFC 8881 Sec 10 |
| **DATA-02** | Shipped | 4 pods append 50 records each with `O_APPEND`: zero torn records; exact line count carries caveat | Implementation caveat: NFSv4.1 lacks atomic append; torn records are corruption |
| **DATA-03** | Shipped | Close-to-open: pod A writes & closes, pod B opens & reads: B sees A's data | RFC 8881 Sec 10 (Client cache consistency) |
| **DATA-04** | Shipped | Negative visibility: pod A writes without closing: reader sees empty, prefix, or full data; never unwritten bytes | Deployment-specific boundary check; immediate visibility after close |
| **DATA-05** | Shipped | Cross-node locking: mutual exclusion holds for whole-file (`flock`) and disjoint byte ranges (`locktool`) | RFC 8881 Sec 9 (Locking & Share reservations) |
| **DATA-06** | Shipped | Pod holding byte-range lock is force-deleted: lock released within one lease period; node unmounts cleanly | Linux client lease model; teardown unmount ordering ([F-001](findings.md)) |
| **DATA-07** | Shipped | Two pods open file with `O_DIRECT`: writes land at disjoint offsets, bypass page cache, reads match written data | Linux `open(2)` `O_DIRECT`, block-aligned I/O |
| **DATA-08** | Shipped | `noac` mount option on clone PV: cross-node reader sees unclosed write immediately | `nfs(5)` attribute caching options |
| **DATA-09** | Shipped | Same-node open unlink creates `.nfs*` silly rename; cross-node unlink returns old data or `ESTALE`; rename follows file | Linux VFS silly-rename semantics, RFC 8881 file handles |
| **DATA-10** | Shipped | 100k entries directory listed while 50k entries deleted: listing completes, 0 invented names; server pod restarts and dmesg error markers monitored | READDIR cache reuse safety under pressure |
| **DATA-11** | Shipped | Sparse file write: holes read as zero, logical size matches; hole punch recorded as unsupported on NFSv4.1 | RFC 7862 (NFSv4.2 `DEALLOCATE` unavailable on 4.1); zero-fill intact |
| **DATA-12** | Shipped | `fsync` durability: write records with `conv=fsync`, SIGKILL server: 100% acknowledged records survive with verdict `correct` | RFC 8881 Sec 18.3 (`COMMIT` durability guarantee) |
| **DATA-13** | Shipped | Uncommitted durability: write records without `fsync`, SIGKILL server: uncommitted records may be absent or short, 0 `wrong` | RFC 8881 Sec 18.3 (Uncommitted writes lack durability) |
| **DATA-14** | Deferred | 20 pods, 70/30 read/write soak for 1h with `fio`: zero checksum mismatches | Deferred to `SCALE-07` soak testing (see Section 6) |

## 5. Detailed case walkthroughs (Shipped cases)

### DATA-01: Concurrent writers to distinct files
- **Steps**:
  1. Schedule four client pods round-robin across schedulable worker nodes on one RWX claim.
  2. Concurrently write a 1MiB file from each pod with a distinct pseudorandom seed.
  3. Verify each file from a pod on a different node to cross the server and avoid local cache hits.
  4. Assert all four checksums match the writer's generated seed and that all four checksums are distinct.
  5. Assert the directory contains exactly four entries.

### DATA-02: Concurrent appends to one shared file (`O_APPEND`)
- **Steps**:
  1. Schedule four appender pods across worker nodes and truncate the shared file.
  2. Concurrently append 50 records (one short line each) per pod through an open file descriptor.
  3. Read back the entire file from a dedicated non-writing reader pod (placed on an unused node when more than 4 workers are available) to cross the server and avoid local cache hits.
  4. Assert every line is a whole, untorn record, and no record is duplicated (torn records fail immediately as corruption).
  5. Check total line count against 200 expected records; if lines were lost, report failure with the implementation caveat and list the missing record IDs per appender ([F-016](findings.md)).

### DATA-03: Close-to-open cache consistency
- **Steps**:
  1. Pin a writer pod to Node A and a reader pod to Node B on one claim.
  2. Writer creates and writes payload using shell redirection (`>`), which closes the descriptor upon completion.
  3. Reader immediately opens and reads the file.
  4. Assert the reader observes the exact payload closed by the writer.

### DATA-04: Negative visibility before close
- **Steps**:
  1. Pin a writer pod to Node A and a reader pod to Node B on one claim.
  2. Writer writes payload through a descriptor held open (`HoldOpenWrite`).
  3. Reader attempts to read the file before writer closes:
     - Reading empty, a partial prefix, or full data is logged as acceptable under close-to-open.
     - Reading characters outside the payload prefix fails immediately as corruption.
  4. Writer closes the file descriptor.
  5. Reader polls until the closed file content is fully visible, asserting bounded latency.

### DATA-05: Cross-node file and byte-range locking
- **Steps**:
  1. Pin holder pod to Node A and contender pod to Node B on one claim.
  2. Verify server-side locking support per subtest: check for `nolock` or `local_lock=flock|all` before whole-file flock, and `nolock` or `local_lock=posix|all` before byte ranges.
  3. Subtest `whole-file-flock`:
     a. Holder acquires exclusive whole-file lock via `HoldFlock`.
     b. Contender probes lock via `TryFlock` and asserts it is refused.
     c. Holder releases lock; contender retries and asserts acquisition succeeds within clean-release bounds.
  4. Subtest `byte-ranges`:
     a. Holder acquires byte range `[0, 4096)` and contender acquires disjoint byte range `[8192, 12288)` on the same file via `locktool`.
     b. Both disjoint acquisitions must succeed simultaneously.
     c. Probing overlapping ranges (`[0, 8192)` or each other's ranges) must be refused, naming the conflicting range.

### DATA-06: Byte-range lock held by force-deleted pod
- **Steps**:
  1. Pin holder pod to Node A and contender pod to Node B on one claim.
  2. Holder acquires byte range `[0, 4096)` on shared file via `locktool hold`.
  3. Force-delete holder pod (`gracePeriodSeconds: 0`) via `ForceDeletePodAt` without altering pod finalizers.
  4. Wait for Node A to release the mount (observed via `NodeStatus.VolumesInUse` if attach-required, or directly via `/proc/mounts` inspection via `AwaitUnmount`), preventing node wedging ([F-001](findings.md)).
  5. Contender attempts to acquire `[0, 4096)` alongside the unmount wait. Assert acquisition succeeds within the profile lock release bound (`slo.LockReleaseBound`).
  6. Classify release mechanism: ~1 second indicates local descriptor cleanup; ~lease duration indicates lease expiry.

### DATA-07: Direct I/O (`O_DIRECT`) from two pods
- **Steps**:
  1. Pin two pods to two nodes on one claim.
  2. Probe `oflag=direct` on both nodes; report `blocked` if tools image dd lacks `FEATURE_DD_IBS_OBS`.
  3. Each pod writes a block-aligned 4KiB block at disjoint offsets (block 0 and block 1) with `O_DIRECT`.
  4. Each pod reads the other's block back using `O_DIRECT` (bypassing page cache on both sides).
  5. Assert both blocks match the writer's checksum.

### DATA-08: Attribute caching bypass (`noac`)
- **Steps**:
  1. Dynamically provision claim and extract export server and path from PV.
  2. Create a static clone PV pointing to the same export with mount option `noac`, and bind a claim to it.
  3. Pin writer to Node A (dynamic claim) and reader to Node B (clone claim with `noac`).
  4. Assert Node B `/proc/mounts` actively carries `noac`.
  5. Writer writes payload through an unclosed open descriptor.
  6. Assert reader on Node B sees the unclosed payload without waiting for a close.

### DATA-09: Silly rename and open file descriptors
- **Steps**:
  1. Subtest `same-node-unlink`:
     a. Pod A on Node A holds open descriptor to `same-node.dat`.
     b. Pod B on Node A unlinks `same-node.dat`.
     c. Assert client creates `.nfs*` silly rename entry in directory.
     d. Assert Pod A continues reading original content through held descriptor.
     e. Pod A closes descriptor; assert `.nfs*` entry is automatically unlinked.
  2. Subtest `cross-node-unlink`:
     a. Pod A on Node A holds open descriptor.
     b. Pod C on Node B unlinks and creates a decoy file with the same name.
     c. Assert Pod A reads either old content or returns an error prefixed with `ReadFailedPrefix` (including `ESTALE` and I/O errors); fail if Pod A reads decoy content (handle reuse) or unexpected bytes.
  3. Subtest `rename`:
     a. File renamed from `old.dat` to `new.dat` while descriptor open; assert descriptor reads original file content.

### DATA-10: Large directory readdir racing concurrent deletions
- **Steps**:
  1. Pre-check free capacity: report `blocked` if export reports fewer than 100,000 free inodes (`FreeInodes < 100000`), while logging free bytes.
  2. Record baseline kernel ring buffer (`dmesg`) on worker nodes and baseline server restart count.
  3. Populate directory with 100,000 files using batched parallel sharding.
  4. Start background process deleting 50,000 files while client pod lists the directory with `find`.
  5. Validate returned entries:
     - Count can legitimately be fewer than 100k due to concurrent deletions.
     - Any returned name not created during population fails immediately as a directory chunk use-after-free defect.
  6. Assert server pod restart count did not increase and no new kernel oops or KASAN error markers appeared in `dmesg`.

### DATA-11: Sparse file write and hole punch
- **Steps**:
  1. Subtest `sparse-write-and-read-back`:
     a. Write block 0, seek past hole, write block 8 (total logical length 9 blocks).
     b. Assert logical size is exactly 36KiB.
     c. Assert the unwritten hole at block 1 (`holeBlock = 1`) reads back strictly as binary zeros (checked via `countNonZero`), and block 8 matches written content.
     d. Record allocated physical blocks via `stat -c %b`.
  2. Subtest `hole-punch`:
     a. Probe whether `fallocate -p` is supported by tools image.
     b. If unsupported, report `blocked` naming `-tools-image`.
     c. If supported, attempt hole punch: on NFSv4.1, any non-zero exit from `fallocate -p` is recorded as an unsupported protocol limitation (RFC 7862 / NFSv4.2 `DEALLOCATE` required). Fail only if punch reports success without zeroing the block.

### DATA-12: `fsync` and `COMMIT` durability across server SIGKILL
- **Steps**:
  1. Start write workload generating 1 record/s with `conv=fsync`.
  2. Workload attempts at least 30 records, requiring at least 10 committed records (`minDurabilitySet: 10`) before fault injection.
  3. Locate server process PID on host node via `ServerTarget` and send `SIGKILL`.
  4. Wait for client I/O to recover (outage measured as silence gap; recovery timing owned by `CHAOS-01`).
  5. Run content-verifying sweep from an independent worker node across all records committed before fault.
  6. Assert 100% of committed records return verdict `correct`. Fail on `absent`, `short`, or `wrong`.

### DATA-13: Uncommitted durability across server SIGKILL
- **Steps**:
  1. Start write workload generating 1 record/s without `fsync` (`NoFsync: true`).
  2. Workload attempts at least 30 records before fault.
  3. Send `SIGKILL` to server process on host node.
  4. Wait for client I/O to recover.
  5. Run content-verifying sweep across all attempted records from independent worker node.
  6. Record number of `absent` and `short` records as lawful uncommitted write loss.
  7. Fail only if any record returns verdict `wrong` (corrupted bytes).

## 6. DATA-14 deferral rationale

`DATA-14` called for a 1-hour mixed 70/30 read/write soak test across 20 pods using `fio` with
CRC32C verification. It was designed, evaluated, and deferred from the data path test group:

1. **Category alignment**: An hour-long 20-pod soak test is a scale and endurance test wearing
   a data path case number. The repository's designated soak test is `SCALE-07` (an 8-hour sustained
   throughput evaluation in test plan Section 3.4). Running two separate soak tests creates redundant,
   uncoordinated evaluations.
2. **Tooling and image dependencies**: `fio` is not part of busybox and cannot be built as a small
   hermetic binary. It requires an external container image (`-fio-image`) that operators must host.
3. **Storage capacity footprint**: 20 pods writing up to 1GiB across 4 files each requires up to 80GiB
   of backing volume capacity, exceeding the footprint of all other test cases combined.
4. **Time budget constraints**: Section 4.2 of the test plan assigns strict budgets (under 45 minutes)
   to each test category. An hour-long case violates category boundaries.

The design for `DATA-14` is preserved for `SCALE-07`: one job per pod over an isolated directory,
`verify_fatal=1` to stop at the exact offset of any mismatch, and pre-run capacity validation.

## 7. Key decisions and invariants

- **`make test-data` injects faults**: `DATA-12` and `DATA-13` kill the server process under active I/O.
  The test suite groups tests strictly by functional category (`TestData`), superseding earlier rules
  that reserved faults to `TestChaos`.
- **Close-to-open, not POSIX**: The harness never asserts instantaneous cross-node visibility for unclosed
  files on standard mounts (`DATA-04`).
- **Four-verdict durability sweep**: Binary existence checks hide truncation and byte corruption. The sweep
  differentiates `correct`, `absent`, `short`, and `wrong`, establishing the precise boundary between
  committed durability (`DATA-12`) and uncommitted prefix flushing (`DATA-13`).
- **Force deletion requires unmount confirmation**: `DATA-06` waits for the node to release the mount
  (via `NodeStatus.VolumesInUse` or direct `/proc/mounts` inspection via `AwaitUnmount`) before proceeding, preventing persistent volume wedging ([F-001](findings.md)).
- **Mount option guards**: Lock cases check `/proc/mounts` and report `blocked` when `nolock` or
  `local_lock` is active, preventing misattribution of configuration choices to server defects.

## 8. What real runs taught

- **[F-001](findings.md) (Unmount before PVC deletion)**: A client on a `hard` mount retries indefinitely
  if its export is removed. Force deletion must wait for unmount confirmation before claims are deleted.
- **[F-006](findings.md) (Busybox flock lacks `-w`)**: Shell scripts cannot use flags missing from
  busybox applets. `locktool` was built to replace brittle shell invocations with concrete Go syscalls.
- **[F-007](findings.md) (Existence is not a check)**: Early durability checks tested whether files were
  non-empty. Real runs revealed that truncated or zeroed records passed existence checks, necessitating
  the 4-verdict content sweep.
- **[F-010](findings.md) (Anonymous st_dev mismatch)**: The Linux NFS client assigns an anonymous
  `st_dev` per mount. Cross-node `/proc/locks` comparisons must match on inode and range rather than device ID.
- **[F-016](findings.md) (O_APPEND race and attribution)**: Concurrent appends from multiple nodes
  intermittently lose records without tearing under heavy load. A failure message must cite the protocol
  limitation and report missing record IDs.

## 9. What changed after this was written

- **Consolidation by test groups.** To align design documentation directly with the
  test groups implemented in `test/e2e/`, the contents of this document have been
  consolidated to authoritatively cover all Data Path cases (`DATA-01` through `DATA-14`),
  unifying early delivery cases (`DATA-01` to `DATA-05`) with `DATA-06` to `DATA-13` and `locktool`.
- **DATA-14 was deferred**, with the flag, the helper, the script and the target
  removed. Section 6.
- **DATA-11's requirement moved into the test plan.** Narrowing the punch from an
  assertion to a record changes what is verified, so Section 3.2's row was
  amended rather than the narrowing living only here.
- **The run happened.** On 2026-09-11, against a three-worker GKE cluster on
  Kubernetes v1.37 with the in-cluster `nfs-server-provisioner`, DATA-05 to
  DATA-09 and DATA-11 to DATA-13 passed along with CHAOS-06; DATA-11's punch half
  reported blocked, which is the documented answer on a busybox image.
- **DATA-10 has since been run, and passes.** Three times: `full-e2e-20260912b`,
  `pr46-data-20260913` and `pr47-data-20260913`, taking between four and five
  minutes each. The export holds 100k entries, and a listing racing 50k
  deletions returned 98–99k of them, which is lawful rather than a defect. This
  supersedes the sentence that stood here saying it was unmeasured; the test
  plan's Section 5.2 carries the runs it came from.
- **F-006 and F-007** came out of this phase: `scripts/lock-probe.sh` passed
  `flock -w` to an applet that has no `-w`, and two cases reported results they
  had not measured. Both are fixed and recorded in [`findings.md`](findings.md).

Still open: whether `locktool` should grow a `dio` subcommand if real images turn
out not to carry `oflag=direct`, and whether any server reclaims a sub-file range
differently from a whole-file one.

## 10. Sources

- [RFC 8881](https://www.rfc-editor.org/rfc/rfc8881.html), NFSv4.1:
  - Section 9: File Locking and Share Reservations (`LOCK`, `LOCKT`, `LOCKU`).
  - Section 10: Client-Side Caching (Close-to-Open consistency).
  - Section 18.3: `COMMIT` and post-fsync durability.
- [RFC 7862](https://www.rfc-editor.org/rfc/rfc7862.html), NFSv4.2:
  - `ALLOCATE`, `DEALLOCATE` (hole punch), and `READ_PLUS`.
- [`fcntl(2)`](https://man7.org/linux/man-pages/man2/fcntl.2.html),
  [`flock(2)`](https://man7.org/linux/man-pages/man2/flock.2.html),
  [`open(2)`](https://man7.org/linux/man-pages/man2/open.2.html) (`O_DIRECT`, `O_APPEND`), and
  [`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html) for `noac`, `nolock`, `local_lock`, and `hard`.
- [Kernel NFS Client Identifier Documentation](https://docs.kernel.org/filesystems/nfs/client-identifier.html):
  Client lease model shared across mounts and pods per node.
- Linux kernel implementation: `fs/nfs/file.c` (`flock` translation), `fs/nfs/fs_context.c` (mount options),
  `fs/nfs/nfs4state.c` (lock state and `EIO` on lost lock).
- Kubernetes [Persistent Volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/)
  and [CSI specification](https://github.com/container-storage-interface/spec/blob/master/spec.md).

