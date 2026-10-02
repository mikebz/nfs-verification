package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mikebz/nfs-verification/pkg/chaos"
	"github.com/mikebz/nfs-verification/pkg/framework"
	"github.com/mikebz/nfs-verification/pkg/slo"
)

// mountFailureReasons are the reasons kubelet and the attach-detach controller
// use when a volume cannot be made ready. Any of them satisfies OBS-04; what
// the case will not accept is silence.
var mountFailureReasons = map[string]bool{
	"FailedMount":        true,
	"FailedAttachVolume": true,
}

// OBS-04: a mount that fails must reach the operator as a Kubernetes Event with
// a reason they can act on. A failure an operator can only find by reading
// kubelet logs on the node is, in practice, a silent failure, and the pod being
// stuck in ContainerCreating says nothing about why.
//
// The failure is manufactured with a volume pointing at an export that does not
// exist, on an address RFC 5737 reserves for documentation. The real export and
// the real server are not touched, so this case cannot disturb anything else in
// the run.
//
// Steps:
//  1. Create a PV pointing at an unroutable export and a claim bound to it by
//     name, so it can bind to nothing else.
//  2. Create a pod on it, without waiting for Ready, since it never will be.
//  3. Wait for a Warning event with a mount failure reason.
//  4. Assert the message names what could not be mounted; a message that omits
//     the cause is logged rather than failed, since the wording is kubelet's.
//  5. Assert no container reported Ready with a volume that never mounted.
//  6. Delete the pod and wait for it to leave the API before teardown reaches
//     the claim.
func TestObsMountFailureSurfacesAsEvent(t *testing.T) {
	f := framework.New(t, "OBS-04")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	pv, claim, err := f.CreateBrokenNFSVolume(ctx, framework.BrokenNFSSpec{Name: "obs04"})
	if err != nil {
		t.Skipf("blocked: this cluster does not allow a static NFS PersistentVolume, so a mount failure "+
			"cannot be manufactured without disturbing the real export: %v", err)
	}
	t.Logf("claim %s is bound to %s, which points at %s:%s and can never mount",
		claim.Name, pv.Name, framework.UnroutableServer, pv.Spec.NFS.Path)

	// The pod is created without waiting for Ready: it never will be. Waiting
	// is what the Event assertion is for.
	spec := toolsPod("stuck", claim.Name, "")
	obj, err := f.PodBuilder(spec)
	if err != nil {
		t.Fatalf("building the pod: %v", err)
	}
	if _, err := f.C.Kube.CoreV1().Pods(framework.Namespace).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the pod: %v", err)
	}

	// Generous, and for a reason: mount.nfs retries before it gives up, and
	// kubelet has its own operation timeout in front of that. The case is
	// asserting that the report arrives at all, not how fast.
	const reportWithin = 8 * time.Minute
	ev, err := f.WaitPodEvent(ctx, spec.Name, reportWithin, func(e corev1.Event) bool {
		return e.Type == corev1.EventTypeWarning && mountFailureReasons[e.Reason]
	})
	if err != nil {
		t.Fatalf("no mount failure was reported on the pod within %s, so a volume that can never mount "+
			"is invisible to anyone who is not reading kubelet logs on the node: %v", reportWithin, err)
	}
	t.Logf("mount failure surfaced as %s/%s: %s", ev.Type, ev.Reason, strings.TrimSpace(ev.Message))

	// Actionable, not merely present. The message has to name what failed, or
	// an operator with fifty pods learns nothing from it.
	msg := ev.Message
	names := []string{pv.Name, claim.Name, spec.Name, "vol0"}
	if !containsAny(msg, names) {
		t.Errorf("the mount failure event names none of %v, so it does not say what could not be mounted: %q",
			names, msg)
	}
	// And it should say something about the failure itself rather than only
	// that something timed out. This is a warning rather than a failure: the
	// wording is kubelet's, and a cluster that reports the volume without the
	// cause is still far better than silence.
	if !containsAny(strings.ToLower(msg), []string{"mount", "attach", "nfs", "timed out", "timeout", "connection"}) {
		t.Logf("the event names the volume but not the failure, which makes triage slower than it should be: %q", msg)
	}

	// The pod must not have been reported Ready with a volume that never
	// mounted, which would be the worse failure: a lie rather than silence.
	pod, err := f.C.Kube.CoreV1().Pods(framework.Namespace).Get(ctx, f.Name(spec.Name), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-reading the pod: %v", err)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Ready {
			t.Errorf("container %s reports Ready while its volume has never mounted", cs.Name)
		}
	}

	// Delete the pod before the fixture tears the claim down. Nothing ever
	// mounted here, so there is no export to pull out from under a live mount,
	// but the ordering rule from F-001 is worth keeping unconditional.
	if err := f.DeletePod(ctx, spec.Name); err != nil {
		t.Fatalf("deleting the pod: %v", err)
	}
	if err := f.WaitPodGone(ctx, spec.Name, framework.DeleteTimeout); err != nil {
		t.Errorf("the pod did not leave the API after its volume failed to mount: %v", err)
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// OBS-03, with OBS-02 folded in (#19): a failover must leave a record an
// operator can find afterwards, in the server's own words, with a timestamp and
// a duration; and the grace period that record describes must be bounded and
// entered once.
//
// The record is the server announcing grace. Entry is the server saying it has
// restarted and is taking reclaims, which RFC 8881 section 8.4.2.1 starts at
// restart; exit is the server saying it accepts new state again; the window
// between them is the failover as the server saw it. Kubernetes Events and
// container start times are not read for the verdict: a pod delete produces
// them on its own, so they show the controller restarted a pod, not that NFS
// failed over, and Events are best effort and expire.
//
// The server's log stream and the files its serving process writes are both
// read, which is why this case needs the node agent; see
// framework.ServerLogFiles. A server that announces grace nowhere the suite can
// read fails this case: an operator on that deployment cannot see grace either.
// Where a server words it differently, -grace-enter-pattern and
// -grace-exit-pattern state the wording.
//
// Steps:
//  1. Start a workload and take a lock that is never released, so the server
//     has state to reclaim and grace means something.
//  2. Delete the server pod and assert the ordinary recovery, keeping the
//     outage the client saw.
//  3. Wait for grace to be observed both entered and left after the fault.
//  4. Fail when it was not, and say which of the two shapes it was: silence,
//     or an entry with no exit; name what was read and what could not be.
//  5. Assert the window has a duration and is inside the grace exit bound.
//  6. Assert grace was entered once, since a second entry for one failover is
//     the re-entry loop.
//  7. Report where the record was found and the window next to the client's
//     outage.
func TestObsGracePeriodIsObservable(t *testing.T) {
	f := framework.New(t, "OBS-03")
	requireCap(t, f.Caps.NodeAgent, "reading the files the NFS server writes needs the privileged node agent")
	ctx, cancel := caseCtx(t, 45*time.Minute)
	defer cancel()

	s := startChaosCase(ctx, t, f, "obs03")
	if s.target.Controller == "" {
		t.Skipf("blocked: server pod %s has no controller, so deleting it would not bring it back", s.target.Pod)
	}

	// Grace exists to let clients reclaim state. A failover with no outstanding
	// state may be over before it starts, and a case that measured that would
	// be measuring nothing.
	holder, err := f.HoldFlock(ctx, s.writer, s.dir+"/obs03.lock", "obs03")
	if err != nil {
		t.Fatalf("taking the lock that gives grace something to reclaim: %v", err)
	}
	f.Defer(func(ctx context.Context) { _ = holder.Release(ctx) })

	faultAt, err := f.PodNow(ctx, s.writer)
	if err != nil {
		t.Fatalf("reading the writer's clock: %v", err)
	}
	since := time.Now()
	if err := chaos.DeleteServerPod(ctx, f, s.target); err != nil {
		t.Skipf("blocked: %v", err)
	}

	outage := assertRecovered(ctx, t, s, faultAt)

	bound := slo.GraceExitBound(profile(t))
	window, obs, ok := waitGraceWindow(ctx, t, f, since, bound+s.budget)
	if !ok {
		if entries := obs.Entries(); len(entries) > 0 {
			t.Fatalf("the server entered grace at %s and was never observed to leave it within %s: %s. "+
				"Grace entered and never left is what a stuck server and a re-entry loop both look like, "+
				"and on this deployment an operator has no way to tell them apart either",
				entries[0].At.UTC().Format(time.RFC3339), bound+s.budget, obs.Describe())
		}
		t.Fatalf("the failover left no record of grace in anything the server wrote, so an operator "+
			"arriving afterwards cannot tell when it happened or how long it lasted: %s. If this server "+
			"words grace differently, pass -grace-enter-pattern and -grace-exit-pattern; metrics are the "+
			"other channel the plan allows, and OBS-07 reads that endpoint, but no convention states what "+
			"a grace series would be called, so no case reads grace from it", obs.Describe())
	}

	if window.Duration() <= 0 {
		t.Fatalf("grace was observed entering and leaving at the same moment (%s), so no duration can be "+
			"measured from it", window)
	}
	t.Logf("the server recorded the failover in %s: grace ran %s on the %s profile, whose configured "+
		"grace period is %s; the client's outage was %s", obs.Entries()[0].Source, window, profile(t).Name,
		profile(t).Grace, outage.Round(time.Second))
	if window.Duration() > bound {
		t.Errorf("grace lasted %s, above the %s bound for the %s profile, which is two lease periods. "+
			"Every client is blocked for the whole of that window, so this is the term that dominates "+
			"the recovery numbers the other cases report",
			window.Duration().Round(time.Second), bound, profile(t).Name)
	}
	if n := len(obs.Entries()); n > 1 {
		t.Errorf("the server entered grace %d times for one failover, which is a grace re-entry loop: %s. "+
			"It presents as a hung client in front of a healthy server, which is why CHAOS-05 needs this "+
			"case to be diagnosable", n, obs.Describe())
	}
}

// OBS-06: a volume near capacity has to be visible to whoever runs the cluster,
// which means the control plane must report this volume's usage, that reading
// must agree with what the workload itself sees, and both must move when the
// workload writes.
//
// Nothing here fills anything. The threshold an alert would sit on is the
// operator's, so approaching it would prove nothing and risk the cluster; what
// is a property of the deployment is whether the input those rules need exists
// at all. Two sources are read: df inside the pod, which is what the
// application gets ENOSPC against, and the kubelet's entry for the same claim,
// which is what monitoring reads. The failure that matters is a control plane
// reporting a volume as nearly empty while the workload has run out of room.
//
// Steps:
//  1. Create an RWX claim and a pod on it, and read what df says inside the pod.
//  2. Wait for a kubelet reading of the same claim at least as new as that one.
//     No reading at all fails the case and names the CSI driver: a volume the
//     control plane cannot measure is one nobody can monitor for capacity.
//  3. Assert the two agree on used bytes within the tolerance in pkg/slo.
//  4. Write a bounded, absolute number of bytes and take both readings again.
//  5. Assert both moved by the bytes that actually landed. A control plane
//     reading that did not move fails: df confirms the bytes are there and the
//     suite knows how many it wrote, so there is nothing else it could be.
//  6. Last, compare what df reports as the total against the claim's capacity,
//     so that everything above is measured and recorded before this can stop
//     the case. An export with no per-volume quota reports the backing
//     filesystem, and two sources agreeing about the wrong filesystem is
//     exactly the shape a passing case would hide.
func TestObsVolumeUsageAgreesWithControlPlane(t *testing.T) {
	f := framework.New(t, "OBS-06")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	pvc := f.MustRWXPVC(ctx, "obs06")
	pod := f.MustPod(ctx, toolsPod("writer", pvc.Name, ""))
	node := pod.Spec.NodeName
	t.Logf("claim %s is mounted by %s on node %s, and the kubelet on that node is the control plane's "+
		"view of it", pvc.Name, pod.Name, node)

	// Read the provisioned capacity now, because the agreement tolerance is a
	// fraction of it. Nothing is asserted about it here: the quota check that
	// compares it against what df reports runs last, after everything else has
	// been measured and recorded.
	bound, err := f.C.Kube.CoreV1().PersistentVolumeClaims(framework.Namespace).
		Get(ctx, pvc.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-reading claim %s for the capacity it was provisioned at: %v", pvc.Name, err)
	}
	capacity := bound.Status.Capacity[corev1.ResourceStorage]
	claimBytes := capacity.Value()

	report := &framework.UsageReport{}
	// Written whether the case passed or not, and before teardown: a run where
	// the two sources differed by 2% is a different run from one where they
	// agreed exactly, and by teardown neither source can be asked again.
	t.Cleanup(func() {
		if err := f.WriteArtifact("volume-usage.txt", []byte(report.Table())); err != nil {
			t.Logf("writing the usage table: %v", err)
		}
	})

	before := agreeOnUsage(ctx, t, f, node, pvc.Name, "before", claimBytes, report)

	written, err := f.WriteBytes(ctx, "writer", fileIn("obs06.bin"), slo.VolumeWriteBytes, "obs06")
	if err != nil {
		t.Fatalf("writing %d bytes through the export: %v", slo.VolumeWriteBytes, err)
	}
	t.Logf("wrote %d bytes of the %d asked for", written, slo.VolumeWriteBytes)
	if written <= 0 {
		blocked(t, "the write reported %d bytes on the claim, so this case has not changed the quantity "+
			"it is about and nothing it could read afterwards would mean anything", written)
	}

	after := agreeOnUsage(ctx, t, f, node, pvc.Name, "after", claimBytes, report)

	// Two sources that agree on a static number prove less than two that move
	// together, so the movement is asserted against the bytes that actually
	// landed rather than against the size that was asked for.
	floor := framework.MovementFloor(written)
	podDelta := framework.UsedDelta(before.Pod, after.Pod)
	kubeletDelta := framework.UsedDelta(before.Kubelet, after.Kubelet)
	t.Logf("after writing %d bytes: the workload's used bytes moved by %d, the control plane's by %d, "+
		"and either has to move by at least %d", written, podDelta, kubeletDelta, floor)

	// The workload's own view not moving is the case failing to establish its
	// own precondition rather than a finding about the deployment: the bytes it
	// meant to write are not where it thinks they are, and nothing downstream
	// of that is worth asserting.
	if podDelta < floor {
		blocked(t, "the workload wrote %d bytes to %s and its own df moved by %d, below the %d this case "+
			"needs to have written before it can ask whether the control plane saw it. Nothing is known "+
			"here about what the control plane publishes", written, pvc.Name, podDelta, floor)
	}
	if kubeletDelta < floor {
		t.Errorf("the workload wrote %d bytes and its own df moved by %d, while the control plane's "+
			"reading of the same claim moved by %d, below the %d floor. The bytes are on the volume, so "+
			"this is a usage figure that does not track the volume it describes: a threshold on it would "+
			"never fire, whatever an operator set it to. Control plane rows: %s then %s",
			written, podDelta, kubeletDelta, floor, before.Kubelet, after.Kubelet)
	}

	// Last, and on purpose: everything above is measured and recorded before
	// this can stop the case.
	if claimBytes <= 0 {
		// Without a capacity on the bound claim there is nothing to compare
		// against, and failing here would name the provisioner for a number the
		// case never read.
		blocked(t, "claim %s is bound and its status carries no capacity, so what df reports as the total "+
			"cannot be checked against what was provisioned", pvc.Name)
	}
	if !framework.MatchesClaimCapacity(after.Pod.CapacityBytes, claimBytes) {
		t.Errorf("claim %s is provisioned at %d bytes and df inside the pod reports a total of %d. The "+
			"export is a subdirectory of a larger filesystem with no per-volume quota, so both sources "+
			"are measuring that filesystem rather than this volume, and no threshold on that number "+
			"describes this claim. This is the provisioner's configuration, not the NFS server: the "+
			"driver behind StorageClass %s is %s, and a quota option it does not have on cannot produce "+
			"a per-volume total. See F-009 in docs/findings.md",
			pvc.Name, claimBytes, after.Pod.CapacityBytes, f.Env.StorageClass, csiDriver(f))
	}
}

// agreeOnUsage reads both views of one claim, waits for a control plane reading
// at least as new as the workload's, and asserts the two agree.
//
// The three ways it can end without a comparison are routed differently on
// purpose. A refused node proxy is blocked: the suite could not reach the
// source, so nothing was learned about the deployment. No reading and a reading
// that never catches up are both failures that name the deployment, because on
// that cluster an operator has no input either and no rule they write will
// change that.
func agreeOnUsage(ctx context.Context, t *testing.T, f *framework.Framework, node, claim, label string,
	claimBytes int64, report *framework.UsageReport) framework.UsageComparison {
	t.Helper()

	podUsage, err := f.ClaimUsage(ctx, "writer", mountPath, claim)
	if err != nil {
		t.Fatalf("reading what the workload sees of %s: %v", claim, err)
	}
	kubeletUsage, err := f.FreshKubeletUsage(ctx, node, "writer", claim, podUsage.At, slo.VolumeStatsFreshness)
	switch {
	case framework.IsBlocked(err):
		blocked(t, "%v", err)
	case errors.Is(err, framework.ErrNoVolumeStats):
		t.Fatalf("%v. Volume statistics are an optional node capability in the CSI specification, and the "+
			"driver %s behind StorageClass %s does not report them for this volume, so how full %s is "+
			"cannot be seen from outside the workload at all. There is no number here for a threshold to "+
			"sit on, which is a property of this deployment rather than of the NFS server",
			err, csiDriver(f), f.Env.StorageClass, claim)
	case errors.Is(err, framework.ErrStaleVolumeStats):
		t.Fatalf("%v. The kubelet recomputes volume statistics every %s by default and this case waited "+
			"%s, so a reading on node %s still behind the workload's means the control plane's view of "+
			"this volume is not being refreshed. A usage figure nobody updates cannot report a volume "+
			"filling up", err, slo.VolumeStatsPeriod, slo.VolumeStatsFreshness, node)
	case err != nil:
		t.Fatalf("reading what the control plane sees of %s on node %s: %v", claim, node, err)
	}

	cmp := framework.CompareUsage(podUsage, kubeletUsage, claimBytes)
	report.Record(label, cmp)
	t.Logf("%s: %s", label, cmp)
	if cmp.Verdict != framework.UsageAgrees {
		t.Errorf("the two views of %s %s at the %s reading. The workload's own df says %s; the control "+
			"plane says %s; they differ by %d bytes against a tolerance of %d, which is %.0f%% of the "+
			"smaller of the claim's %d bytes and the capacity the workload is shown. An operator watching "+
			"the control plane's number is not watching the volume the application is writing to",
			claim, cmp.Verdict, label, cmp.Pod, cmp.Kubelet, cmp.DeltaBytes, cmp.ToleranceBytes,
			slo.VolumeUsageTolerance*100, claimBytes)
	}
	return cmp
}

// csiDriver names the driver behind the class under test, for a message that
// has to say whose configuration produced a finding. Preflight takes it from
// the StorageClass; a record without one still has to produce a readable
// sentence rather than a gap where the owner should be.
func csiDriver(f *framework.Framework) string {
	if f.Env.CSIDriver == "" {
		return "(not recorded by preflight)"
	}
	return f.Env.CSIDriver
}

// OBS-07: the server's own metrics must answer before a restart and after it,
// and its counters must either carry across or reset cleanly.
//
// This is the only channel that says anything about NFS itself: operation and
// error rates, clients holding state, locks held, open files. The kubelet
// reports a container and the Kubernetes API reports a pod, and a failover is
// exactly the moment an operator needs more than either. A server that
// publishes nothing fails the case rather than skipping it, per Section 3.5 of
// the test plan: a deployment that says nothing about NFS has nothing that
// could survive anything, and no container-level signal is accepted in its
// place.
//
// Nothing here is a protocol guarantee. No RFC requires an NFS server to
// publish metrics, so every failure below says it is reporting what the test
// plan requires of a deployment, not a defect in NFS.
//
// Both deployments this has been run against publish nothing as shipped, which
// is F-023 in docs/findings.md, and on those the case stops at step 2 with the
// absent verdict. It has been run end to end only against gke-w2 with Ganesha's
// exposer switched on by hand: see F-023 for what that took, F-024 for the
// false pass the first such run produced, and F-025 for the false failure the
// whole-suite runs produced after it. The red stays red on an unconfigured
// deployment; see F-023 for what is and is not being claimed by an absent
// verdict.
//
// When the name comparison is made is the half of this case F-025 is about.
// Ganesha creates a metric family on its first sample, so a process that has
// served nothing publishes a cold-start set, and a scrape of it reads every
// byte, size and cache family as lost. The case therefore drives its own I/O
// through the export on both sides of the restart and judges names only once it
// has. The counters are read off the replacement's first answer instead,
// before that traffic, because the case's own writes would push them upward and
// a restarted process would read as one that kept its counts (F-024).
//
// The gap in the series across the outage is not a defect and is not asserted
// on. Neither is the exact label set: per-client and per-export labels come and
// go with the clients and exports themselves, and some statuses, NFS4ERR_GRACE
// among them, exist only because an earlier failover produced them. What must
// survive is the metric name, which is what a dashboard query or an alert rule
// selects. The case does say which label sets it expects back, the ones its own
// traffic touched before the fault, and reports those that did not return
// apart from the rest. It does not assert on them either: whether a read in a
// pod reaches the server at all is the Linux client's decision, made from its
// cache under close-to-open consistency (nfs(5)), so a series such as a READ
// count can be touched on one side of the restart and not on the other with
// nothing lost, and failing on it would file the client's caching against the
// server.
//
// Steps:
//  1. Find the server pod, and register the bundle against it before anything
//     else, since every exit below leaves a result worth the evidence and the
//     pod is gone by teardown.
//  2. Read where it says its metrics are: the prometheus.io annotations a
//     scraper reads, or a container port named for metrics.
//  3. Provision an RWX claim and a pod on it. Scrape, drive one round of
//     traffic through the export (write and sync a file, stat it, read it
//     back), and scrape again. What moved between the two scrapes is what the
//     case's traffic touches. No endpoint, an endpoint that does not answer, or
//     a body that is not the exposition format all fail here, each saying
//     which it was.
//  4. Record which server pods exist, then delete the target and wait for a
//     replacement to be ready.
//  5. Scrape the replacement, which is a pod that was not in step 4's set,
//     until it answers, inside the restart budget. Counters are read from this
//     answer.
//  6. Drive the same traffic through the same mount and scrape again, round
//     after round, until every metric name from step 3 is back or a round
//     brings back no name the one before it had not, inside the same budget.
//  7. Fail on a metric name published before the restart and in neither scrape
//     after it. Pass on counters reset, carried across or unchanged, saying
//     which happened.
func TestObsMetricsSurviveServerRestart(t *testing.T) {
	f := framework.New(t, "OBS-07")
	ctx, cancel := caseCtx(t, 30*time.Minute)
	defer cancel()

	target, err := chaos.ServerTarget(ctx, f)
	if err != nil {
		t.Skipf("blocked: %v", err)
	}
	if target.Controller == "" {
		t.Skipf("blocked: server pod %s has no controller, so deleting it would not bring it back, and "+
			"the case would be asking whether metrics survived a permanent outage", target.Pod)
	}
	pod, err := f.C.Kube.CoreV1().Pods(target.Namespace).Get(ctx, target.Pod, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-reading server pod %s/%s for what it declares about metrics: %v",
			target.Namespace, target.Pod, err)
	}

	// The bundle is registered here, before anything can end the case, and not
	// after the first scrape. Every exit below this line is a result somebody
	// will want the evidence for, and the two that need it most are the ones
	// that have no scrape to carry it: the endpoint is gone by teardown, and an
	// absent verdict's whole content is what the pod declared instead. Writing
	// it only once there was a scrape meant the one outcome this case has
	// actually produced on real hardware, F-023, recorded nothing at all.
	// Reported as a review finding on PR #74.
	var report framework.MetricsComparison
	source := framework.DescribePodPorts(pod)
	t.Cleanup(func() {
		report.Source = source
		if err := f.WriteArtifact("server-metrics.txt", []byte(report.Table())); err != nil {
			t.Logf("writing the metrics comparison: %v", err)
		}
	})

	endpoint, ok := framework.MetricsEndpointOf(pod)
	if !ok {
		// Named rather than left as "not reached". This is the outcome F-023
		// and the design doc both call absent, and a bundle that declined to
		// name it would be the only place in the run that did not.
		report.Verdict = framework.MetricsAbsent
		t.Fatalf("the NFS server publishes no metrics endpoint, so nothing it knows about NFS leaves "+
			"the process: %s. An operator on this deployment can see that a container restarted and "+
			"not that a server failed over, cannot see operation or error rates, and cannot see which "+
			"clients hold state. Two channels were read: the prometheus.io annotations, which are the "+
			"convention the common charts and collection stacks emit rather than a specification, and "+
			"a container port named for metrics. If this server marks its scrape targets under some "+
			"other annotation name, it is in the list above and this verdict is wrong; otherwise no "+
			"scraper following the convention would find an endpoint here either. This is what the "+
			"test plan requires of a deployment (Section 3.5), not an NFS protocol guarantee. If this "+
			"is an NFS-Ganesha deployment, F-023 in docs/findings.md has the two gates and how to "+
			"check them: the build needs USE_MONITORING, and Enable_Metrics defaults to false even "+
			"when it is present, so an endpoint can be one config line away rather than absent",
			source)
	}
	source = fmt.Sprintf("%s, declared by %s", endpoint, framework.DescribePodPorts(pod))
	t.Logf("server pod %s on %s says its metrics are at %s", target.Pod, target.Node, endpoint)

	// The claim is provisioned only now, after the endpoint check, so that a
	// deployment with nothing to scrape fails exactly as it did before this
	// case drove any traffic (F-023), without a volume created for nothing.
	pvc := f.MustRWXPVC(ctx, "obs07")
	client := f.MustPod(ctx, toolsPod("client", pvc.Name, ""))
	clientNode := client.Spec.NodeName
	t.Logf("claim %s is mounted by %s on node %s, and every round of traffic below goes through that export",
		pvc.Name, client.Name, clientNode)

	// Scraped straight into the report the cleanup writes, not into locals
	// copied across afterwards: scrapeBeforeFault ends the case on a body with
	// no series, and that body is the evidence the bundle has to keep.
	scrapeBeforeFault(ctx, t, f, endpoint, &report.Idle, "before this case drove any traffic")
	if err := driveExportTraffic(ctx, f, "client", 0); err != nil {
		t.Fatalf("driving traffic through claim %s from %s on node %s before any fault was injected: %v. "+
			"Nothing was restarted yet, so this is the export failing ordinary I/O, not anything about "+
			"metrics", pvc.Name, client.Name, clientNode, err)
	}
	scrapeBeforeFault(ctx, t, f, endpoint, &report.Before, "after this case drove its traffic")
	idle, before := report.Idle, report.Before
	t.Logf("before the restart: %s", before.Describe())

	budget, err := slo.Recovery(profile(t), slo.EventServerRestart)
	if err != nil {
		t.Fatalf("%v", err)
	}

	// Every server pod that exists before the fault, not just the target. On a
	// deployment serving this class from more than one pod, a sibling that was
	// already up satisfies "ready and not the pod we deleted", and scraping it
	// would report metrics as having survived a restart that was never
	// observed: the case would pass without reading the thing it is about.
	// Reported as a review finding on PR #74.
	preFault, err := serverPodUIDs(ctx, f)
	if err != nil {
		t.Fatalf("listing the server pods before the fault: %v", err)
	}

	if err := chaos.DeleteServerPod(ctx, f, target); err != nil {
		t.Skipf("blocked: %v", err)
	}
	// Waited out past the budget on purpose, and not asserted against it: what
	// this case is about is whether the endpoint comes back at all, and CHAOS
	// owns how long a restart takes.
	within := budget + slo.ObservationMargin
	if err := chaos.WaitServerReplaced(ctx, f, target, within); err != nil {
		t.Fatalf("no replacement server pod became ready within %s of deleting %s, so there is nothing "+
			"to scrape and nothing this case can say about whether metrics survive a restart: %v",
			within, target.Pod, err)
	}

	after, afterEndpoint, afterPod, resumed := waitMetricsBack(ctx, t, f, preFault, within)
	// Which process each scrape came from, by UID, in the bundle. A
	// StatefulSet's replacement reuses the pod's name, so the name alone cannot
	// show that the second scrape read a different process, and that is the
	// first thing anyone doubting the verdict will want. F-024 was diagnosed by
	// going back to the cluster for this, which only worked because the pod was
	// still there; after teardown it would not have been.
	source = fmt.Sprintf("%s. Scraped before from %s/%s, after from %s",
		source, pod.Name, pod.UID, afterPod)
	if !resumed {
		report = framework.ClassifyMetrics(framework.MetricScrapes{Idle: &idle, Before: &before})
		t.Fatalf("the metrics endpoint published %d series before the restart and nothing answered "+
			"at it within %s afterwards, while a replacement server pod is ready. Every dashboard "+
			"and every alert built on those series goes dark at the moment it is needed, which is "+
			"the failover. The endpoint was %s",
			before.Len(), within, endpoint)
	}
	t.Logf("after the restart, %s: %s", afterPod, after.Describe())
	report.After = after

	exercised, judgedAfter, rounds, err := exerciseReplacement(ctx, t, f, afterEndpoint, "client", &report, within)
	if exercised == nil {
		// No verdict is recorded, and report keeps whatever body the last
		// scrape returned. Judging names against the first answer alone
		// is the comparison F-025 showed cannot tell a lost name from one the
		// server has not recorded yet, and a never-resumed verdict in the
		// bundle from it would say something this run did not observe.
		t.Fatalf("this case's own traffic through claim %s from %s on node %s did not complete and get "+
			"scraped within %s of the replacement answering, on the %s profile, after %d rounds: %v. "+
			"Without it the case cannot tell a metric name this server lost from one it has not yet "+
			"had a sample for (F-025), so it says nothing about the names. An export that does not "+
			"serve a write this long after the server came back is a recovery failure, which is what "+
			"the CHAOS cases measure",
			pvc.Name, client.Name, clientNode, within, profile(t).Name, rounds, err)
	}
	if err != nil {
		t.Logf("the rounds ended at the %s bound rather than settling, after %d rounds, so the names are "+
			"judged on the scrape that followed round %d, the last with series: %v",
			within, rounds, judgedAfter, err)
	}
	t.Logf("after %d rounds of traffic through the replacement: %s", judgedAfter, exercised.Describe())

	report = framework.ClassifyMetrics(framework.MetricScrapes{
		Idle: &idle, Before: &before, After: &after, Exercised: exercised,
	})
	report.Rounds = judgedAfter
	t.Logf("%s", report)

	switch report.Verdict {
	case framework.MetricsNeverResumed:
		t.Errorf("after the restart, and after %d rounds of this case's traffic through claim %s from %s on "+
			"node %s, the server publishes %d of the %d metric names it published before, and %d are "+
			"gone: %v. Each round wrote, synced, stat'd and read back a file through the export, which is "+
			"what brings back the byte, size and cache families a fresh process has not recorded yet "+
			"(F-025), and the last round brought back nothing the one before it had not. So these are "+
			"not names waiting for their first sample of that kind: either the server stopped publishing "+
			"them, or they are fed only by requests this traffic does not make, such as locks or failover "+
			"recovery, and the before scrape in the bundle shows what they held. A name that stops being "+
			"published across a failover takes every rule written on it with it. Scraped from %s on the %s "+
			"profile. This is what the test plan requires of a deployment (Section 3.5), not an NFS "+
			"protocol guarantee",
			judgedAfter, pvc.Name, client.Name, clientNode, len(before.MetricNames())-len(report.LostMetricNames),
			len(before.MetricNames()), len(report.LostMetricNames), report.LostMetricNames, afterPod,
			profile(t).Name)
	case framework.MetricsResumedReset:
		t.Logf("the endpoint came back with %d counters reset, %d advanced and %d unchanged in its first "+
			"answer, which is what a restarted process is supposed to look like and what every monitoring "+
			"system knows how to read", len(report.Reset), len(report.Advanced), len(report.Unchanged))
	case framework.MetricsResumedContinuous:
		t.Logf("the endpoint came back with %d counters strictly higher than before and none lower in its "+
			"first answer, before this case drove anything through it, so this server carries counts "+
			"across a restart (%d more were unchanged)",
			len(report.Advanced), len(report.Unchanged))
	case framework.MetricsResumedIndeterminate:
		t.Logf("the endpoint came back publishing every metric name it did before, and all %d counters "+
			"its first answer shares with the scrape before the restart are identical. No claim is made "+
			"about continuity: on an idle server a restarted process re-derives the same values, so this "+
			"reading is equally consistent with a reset. See F-024. The counters are read before this "+
			"case's own traffic on purpose, since that traffic would move them upward",
			len(report.Unchanged))
	default:
		t.Errorf("the comparison produced the verdict %q, which this case has no reading for", report.Verdict)
	}

	// Diagnostics, deliberately not assertions; the case comment says why the
	// label sets the case's own traffic touched are not asserted either.
	if len(report.NewMetricNames) > 0 {
		t.Logf("%d metric names appeared only after the restart: %v",
			len(report.NewMetricNames), report.NewMetricNames)
	}
	if len(report.LostTouchedSeries) > 0 {
		t.Logf("%d of the %d series this case's own traffic touched before the fault did not come back "+
			"after it drove the same traffic through the replacement: %v. Not asserted, because the "+
			"Linux client decides from its cache whether a request reaches the server at all, but these "+
			"are the label sets to read first",
			len(report.LostTouchedSeries), len(report.Touched), report.LostTouchedSeries)
	}
	if n := len(report.LostSeries) - len(report.LostTouchedSeries); n > 0 {
		t.Logf("%d more series under a surviving metric name did not come back, none of which this case's "+
			"traffic touched before the fault: history this process was never asked to recreate, such as "+
			"clients and exports from earlier cases and statuses only a failover produces. The full list "+
			"is in the bundle", n)
	}
}

// scrapeBeforeFault takes one of OBS-07's scrapes before the fault into dst,
// and ends the case when there is nothing to compare against.
//
// dst is filled before any check can end the case, so that the bundle holds
// what came back even when what came back is the reason for the failure.
func scrapeBeforeFault(ctx context.Context, t *testing.T, f *framework.Framework, e framework.MetricsEndpoint,
	dst *framework.MetricSet, when string) {
	t.Helper()
	m, err := framework.ScrapeMetrics(ctx, f.C, e)
	switch {
	case framework.IsBlocked(err):
		blocked(t, "%v", err)
	case err != nil:
		t.Fatalf("the server declares a metrics endpoint and it did not answer %s, before any fault was "+
			"injected: %v. A declared endpoint nothing can reach is the same gap as no endpoint, and "+
			"the scraper an operator runs here would record the same thing", when, err)
	}
	*dst = m
	if m.Len() == 0 {
		t.Fatalf("the metrics endpoint answered %s and published no series: %s. Nothing was restarted yet, "+
			"so this is what an operator's scraper collects from this deployment at rest", when, m.Describe())
	}
}

// driveExportTraffic is one round of the I/O OBS-07 puts through the export
// under test: write a file and sync it, stat it, and read it back.
//
// Those are the write, read and metadata paths, which between them feed the
// byte counters, the request and response size histograms and the metadata
// cache counters F-025 found missing from a fresh process. Whether the read
// reaches the server is the client's decision (nfs(5), close-to-open), which is
// why nothing here depends on it doing so. Each round writes its own file, so
// that every round creates one rather than overwriting the last and the rounds
// either side of the restart do the same operations.
//
// The files sit at the top of the mount, not in a directory of their own. The
// first run on gke-w2 put them in obs07/, and WriteBytes' mkdir -p then sent a
// CREATE in the round before the fault and in none after it, so 35 CREATE
// series showed as touched and not brought back with nothing lost. The two
// sides have to do the same operations for the expectation to mean anything.
func driveExportTraffic(ctx context.Context, f *framework.Framework, pod string, round int) error {
	path := fileIn(fmt.Sprintf("obs07-round-%d.bin", round))
	n, err := f.WriteBytes(ctx, pod, path, slo.MetricsTrafficBytes, fmt.Sprintf("obs07-round-%d", round))
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if n != slo.MetricsTrafficBytes {
		return fmt.Errorf("%s holds %d bytes after writing %d", path, n, slo.MetricsTrafficBytes)
	}
	if _, err := f.Sha256(ctx, pod, path); err != nil {
		return fmt.Errorf("reading %s back: %w", path, err)
	}
	return nil
}

// exerciseReplacement drives OBS-07's traffic through the replacement server
// and scrapes it after every round, until every metric name published before
// the restart is back or a round brings back no name the round before it had
// not. It returns the last scrape with series taken after a completed round,
// nil if there was none, how many rounds preceded that scrape, and how many
// rounds completed in all. The two counts differ when a later round's scrape
// failed or came back empty, and the bundle has to name the round the judged
// scrape actually followed.
//
// Every scrape that returns a body, series or not, is written into
// report.Exercised with its round as it is taken, since report is what the
// cleanup writes: when no scrape had series, the body that came back is the
// evidence, and a bundle saying no scrape was taken would be wrong. The same
// rule scrapeBeforeFault follows, from the review of PR #116.
//
// Why rounds rather than one: Ganesha records a sample as it serves the
// request, so one round should be enough, but the scrape can race the counter
// update, and a second round costs nothing on the pass path. Why stop on no
// growth rather than at the bound: once a round brings back nothing new, more
// of the same traffic has nothing further to recreate, and running it until
// the bound would only delay a failure that is already decided. Neither choice
// can turn a lost name into a pass: a name comes back only if the server
// publishes it.
//
// Bounded by within, the same restart budget plus margin the replacement and
// its first answer were given. The first round meets grace, since the server
// refuses new opens until grace ends, so a budget below grace would fail a
// server that is behaving lawfully.
func exerciseReplacement(ctx context.Context, t *testing.T, f *framework.Framework, e framework.MetricsEndpoint,
	pod string, report *framework.MetricsComparison, within time.Duration) (*framework.MetricSet, int, int, error) {
	t.Helper()
	// The rounds run on a mount that is hard, so a server that never comes back
	// would hold an exec forever. The context is what stops it at the bound.
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()

	var exercised *framework.MetricSet
	var refused, last error
	rounds, judgedAfter := 0, 0
	err := framework.Poll(ctx, framework.PollInterval, within, func(ctx context.Context) (bool, error) {
		if err := driveExportTraffic(ctx, f, pod, rounds+1); err != nil {
			last = fmt.Errorf("round %d did not complete: %w", rounds+1, err)
			return false, last
		}
		rounds++
		m, err := framework.ScrapeMetrics(ctx, f.C, e)
		if framework.IsBlocked(err) {
			refused = err
			return true, nil
		}
		if err != nil {
			last = fmt.Errorf("scraping after round %d: %w", rounds, err)
			return false, last
		}
		report.Exercised, report.Rounds = m, rounds
		if m.Len() == 0 {
			last = fmt.Errorf("after round %d: %s", rounds, m.Describe())
			return false, last
		}
		grew := exercised == nil || publishesNewName(m, *exercised)
		exercised, judgedAfter = &m, rounds
		lost := framework.ClassifyMetrics(framework.MetricScrapes{
			Before: &report.Before, After: &report.After, Exercised: &m,
		}).LostMetricNames
		if len(lost) == 0 || !grew {
			return true, nil
		}
		last = fmt.Errorf("%d metric names from before the restart were still missing after round %d",
			len(lost), rounds)
		return false, last
	})
	if refused != nil {
		blocked(t, "%v", refused)
	}
	if err != nil && last != nil {
		// Poll reports the context rather than the round when the bound falls
		// mid-exec, and the round is what a reader needs.
		err = fmt.Errorf("%w; the last round said: %v", err, last)
	}
	return exercised, judgedAfter, rounds, err
}

// publishesNewName reports whether m carries a metric name prev did not.
func publishesNewName(m, prev framework.MetricSet) bool {
	had := map[string]bool{}
	for _, name := range prev.MetricNames() {
		had[name] = true
	}
	for _, name := range m.MetricNames() {
		if !had[name] {
			return true
		}
	}
	return false
}

// serverPodUIDs is the set of server pods that exist right now, taken before a
// fault so that afterwards a replacement can be told from a pod that was there
// all along. UIDs rather than names, because a StatefulSet reuses the name.
func serverPodUIDs(ctx context.Context, f *framework.Framework) (map[types.UID]bool, error) {
	pods, err := framework.ServerPods(ctx, f.C)
	if err != nil {
		return nil, err
	}
	out := make(map[types.UID]bool, len(pods))
	for i := range pods {
		out[pods[i].UID] = true
	}
	return out, nil
}

// waitMetricsBack scrapes the replacement server pod until its metrics endpoint
// answers with something, and reports whether it ever did.
//
// The replacement is identified as a pod that did not exist before the fault,
// by UID, which is two exclusions at once. A StatefulSet's replacement carries
// the same name as the pod it replaces, so matching by name would scrape the one
// on its way out and report metrics as having survived a restart the suite had
// not yet observed: the same confusion as F-013 in docs/findings.md. And on a
// deployment serving this class from several pods, a sibling that was up the
// whole time is ready and is not the deleted pod, so excluding only the target
// would scrape a pod that was never restarted and pass the case without reading
// the thing it is about.
//
// A replacement that declares no endpoint, or one that answers with a body that
// is not the exposition format, keeps the loop going rather than ending it: the
// kubelet reports a pod ready before the process inside it has opened its
// listener, and giving up on the first refused connection would report a
// healthy server as one whose metrics never came back.
//
// Every post-fault pod is tried in each round, not just the first one found.
// A candidate that declares no endpoint, or that is not answering yet, only
// rules itself out: on a deployment that brings up more than one pod, or during
// a rollout, the first candidate the list returns may be a sibling with nothing
// to scrape while the replacement next to it is already serving. Stopping the
// round there would spend the whole budget on the wrong pod and report the
// metrics as never having come back. What each candidate said is kept, so the
// message on a real timeout names them rather than only the last one.
//
// The pod it settled on is returned as "name/uid", because the name alone is
// the same on both sides of a StatefulSet restart and so cannot evidence the
// replacement it just went to the trouble of identifying. Its endpoint is
// returned too, so that every later scrape reads the same replacement.
func waitMetricsBack(ctx context.Context, t *testing.T, f *framework.Framework, preFault map[types.UID]bool,
	within time.Duration) (framework.MetricSet, framework.MetricsEndpoint, string, bool) {
	t.Helper()
	var set framework.MetricSet
	var endpoint framework.MetricsEndpoint
	var pod string
	var refused error
	err := framework.Poll(ctx, framework.PollInterval, within, func(ctx context.Context) (bool, error) {
		pods, err := framework.ServerPods(ctx, f.C)
		if err != nil {
			return false, err
		}
		var why []string
		for i := range pods {
			p := &pods[i]
			if preFault[p.UID] || !framework.PodReady(p) {
				continue
			}

			e, ok := framework.MetricsEndpointOf(p)
			if !ok {
				why = append(why, fmt.Sprintf("%s declares no metrics endpoint: %s",
					p.Name, framework.DescribePodPorts(p)))
				continue
			}
			m, err := framework.ScrapeMetrics(ctx, f.C, e)
			if framework.IsBlocked(err) {
				// A permission the kubeconfig does not have will not arrive
				// within the timeout, so the loop stops rather than spending
				// the budget to report the same thing.
				refused = err
				return true, nil
			}
			if err != nil {
				why = append(why, fmt.Sprintf("%s: %v", p.Name, err))
				continue
			}
			if m.Len() == 0 {
				why = append(why, fmt.Sprintf("%s: %s", p.Name, m.Describe()))
				continue
			}
			set, endpoint, pod = m, e, fmt.Sprintf("%s/%s", p.Name, p.UID)
			return true, nil
		}
		if len(why) == 0 {
			return false, fmt.Errorf("no replacement server pod is ready to scrape yet")
		}
		return false, fmt.Errorf("no replacement server pod answered: %s", strings.Join(why, "; "))
	})
	if refused != nil {
		blocked(t, "%v", refused)
	}
	if err != nil {
		t.Logf("the metrics endpoint did not answer within %s of the restart: %v", within, err)
		return framework.MetricSet{}, framework.MetricsEndpoint{}, "", false
	}
	return set, endpoint, pod, true
}
