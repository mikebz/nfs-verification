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

// OBS-02: a failover must be visible to whoever runs the cluster, with a
// timestamp and a duration. A failover only the client noticed is an
// observability defect: the operator is left with a stalled workload and no
// record of what happened underneath it.
//
// Two channels are read, and either satisfies the case. The server's own log
// stream says NFS failed over; the Kubernetes API says a pod restarted, which
// is less, but it is still something an operator can see. Both are recorded,
// because the difference matters when the server is one pod among many.
//
// Steps:
//  1. Start a workload and delete the server pod.
//  2. Assert the ordinary recovery, and keep the outage the client experienced.
//  3. Read the server's log stream from the fault onwards.
//  4. Read the Kubernetes API for a server container that started after it.
//  5. Fail when neither channel produced a timestamp, since the failover was
//     then invisible from outside the client.
//  6. Report each channel's duration next to the client's outage, and say so
//     when only Kubernetes noticed.
func TestObsFailoverIsObservable(t *testing.T) {
	f := framework.New(t, "OBS-02")
	ctx, cancel := caseCtx(t, 45*time.Minute)
	defer cancel()

	s := startChaosCase(ctx, t, f, "obs02")
	if s.target.Controller == "" {
		t.Skipf("blocked: server pod %s has no controller, so deleting it would not bring it back", s.target.Pod)
	}

	faultAt, err := f.PodNow(ctx, s.writer)
	if err != nil {
		t.Fatalf("reading the writer's clock: %v", err)
	}
	// The reference for both channels below. It is the workstation's clock,
	// while the timestamps the channels carry are the server node's, so the
	// durations here are reported rather than asserted against an SLO. What is
	// asserted is that a duration can be produced at all.
	since := time.Now()
	if err := chaos.DeleteServerPod(ctx, f, s.target); err != nil {
		t.Skipf("blocked: %v", err)
	}

	outage := assertRecovered(ctx, t, s, faultAt)

	lines, sources, logErr := framework.ServerLog(ctx, f.C, since)
	if logErr != nil {
		t.Logf("could not read the server log stream: %v", logErr)
	}
	said, serverSpoke := framework.FirstDated(lines)
	start, kubeSaw, err := framework.ServerStartedAfter(ctx, f.C, since)
	if err != nil {
		t.Logf("could not read the server pods back from the API: %v", err)
	}

	switch {
	case !serverSpoke && !kubeSaw:
		t.Errorf("the failover left no timestamped trace an operator could find: the server printed nothing "+
			"after the fault in %v, and no server container reports having started since. The client saw a "+
			"%s outage, so something happened; an operator watching this deployment would see a stalled "+
			"workload and no record of why",
			sources, outage.Round(time.Second))
	case !serverSpoke:
		t.Logf("only Kubernetes noticed: container in %s on %s started %s after the fault, and the server "+
			"itself printed nothing. That tells an operator a pod restarted, not that NFS failed over, "+
			"which is thin when the server is one pod among many",
			start.Pod, start.Node, start.At.Sub(since).Round(time.Second))
	default:
		t.Logf("the server spoke %s after the fault: %q (from %s)",
			said.At.Sub(since).Round(time.Second), said.Text, said.Source)
	}
	if kubeSaw {
		t.Logf("Kubernetes reports the replacement container in %s on %s started %s after the fault",
			start.Pod, start.Node, start.At.Sub(since).Round(time.Second))
	}
	t.Logf("the client's own outage was %s, measured from the writer pod's clock at both ends",
		outage.Round(time.Second))

	// Grace is the OBS-03 assertion, not this one, but a grace line in the
	// window is the strongest thing a server can say about a failover, so it is
	// worth recording here.
	observeGrace(ctx, t, f, since)
}

// OBS-03: grace entry and exit must both be observable, and the window between
// them measurable. This is the case the plan says CHAOS-05 needs to be
// diagnosable at all: without it, a grace re-entry loop and a hung client look
// the same, and the triage runbook calls that the most common wrong diagnosis
// in this architecture.
//
// A server that says nothing about grace fails this case. That is the finding
// rather than a harness gap: an operator on that deployment cannot see grace
// either. Where a server words it differently, -grace-enter-pattern and
// -grace-exit-pattern state the wording.
//
// Steps:
//  1. Start a workload and take a lock that is never released, so the server
//     has state to reclaim and grace means something.
//  2. Delete the server pod and assert the ordinary recovery.
//  3. Wait for grace to be observed both entered and left.
//  4. Fail when nothing was observed, and say which of the two shapes it was:
//     silence, or an entry with no exit.
//  5. Assert the window is measurable and inside the grace exit bound.
//  6. Assert grace was entered once, since a second entry for one failover is
//     the re-entry loop.
func TestObsGracePeriodIsObservable(t *testing.T) {
	f := framework.New(t, "OBS-03")
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

	assertRecovered(ctx, t, s, faultAt)

	bound := slo.GraceExitBound(profile(t))
	window, obs, ok := waitGraceWindow(ctx, t, f, since, bound+s.budget)
	if !ok {
		if entries := obs.Entries(); len(entries) > 0 {
			t.Fatalf("the server entered grace at %s and was never observed to leave it within %s. "+
				"Grace entered and never left is what a stuck server and a re-entry loop both look like, "+
				"and on this deployment an operator has no way to tell them apart either",
				entries[0].At.UTC().Format(time.RFC3339), bound+s.budget)
		}
		t.Fatalf("the server's log stream says nothing about grace across a failover, so grace entry and "+
			"exit are not observable on this deployment: %s. An operator here cannot answer the third "+
			"question in the triage runbook. If this server words grace differently, pass "+
			"-grace-enter-pattern and -grace-exit-pattern; metrics are the other channel the plan "+
			"allows, and OBS-07 reads that endpoint, but no convention states what a grace series "+
			"would be called, so no case reads grace from it", obs.Describe())
	}

	if window.Duration() <= 0 {
		t.Fatalf("grace was observed entering and leaving at the same moment (%s), so no duration can be "+
			"measured from it", window)
	}
	t.Logf("grace ran %s on the %s profile, whose configured grace period is %s",
		window, profile(t).Name, profile(t).Grace)
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
// This case has never got past step 2 on a real cluster: no endpoint was ever
// discovered, so nothing below it has run outside a unit test. Both deployments
// it has been run against declare no endpoint at all, which is F-023 in
// docs/findings.md; the scrape, the restart and the classification are covered
// by unit tests and not yet by a run. The red stays red: see F-023 for what is
// and is not being claimed by an absent verdict.
//
// The gap in the series across the outage is not a defect and is not asserted
// on. Neither is the exact label set: per-client and per-export labels come and
// go with the clients and exports themselves, so what must survive is the
// metric family, which is what a dashboard or an alert names.
//
// Steps:
//  1. Find the server pod, and register the bundle against it before anything
//     else, since every exit below leaves a result worth the evidence and the
//     pod is gone by teardown.
//  2. Read where it says its metrics are: the prometheus.io annotations a
//     scraper reads, or a container port named for metrics.
//  3. Scrape it. No endpoint, an endpoint that does not answer, or a body that
//     is not the exposition format all fail here, each saying which it was.
//  4. Record which server pods exist, then delete the target and wait for a
//     replacement to be ready.
//  5. Scrape the replacement, which is a pod that was not in step 4's set,
//     until it answers, inside the restart budget.
//  6. Fail on a family published before the restart and not after. Pass on
//     counters reset and on counters carried across, saying which happened.
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
			"test plan requires of a deployment (Section 3.5), not an NFS protocol guarantee",
			source)
	}
	source = fmt.Sprintf("%s, declared by %s", endpoint, framework.DescribePodPorts(pod))
	t.Logf("server pod %s on %s says its metrics are at %s", target.Pod, target.Node, endpoint)

	before, err := framework.ScrapeMetrics(ctx, f.C, endpoint)
	switch {
	case framework.IsBlocked(err):
		blocked(t, "%v", err)
	case err != nil:
		t.Fatalf("the server declares a metrics endpoint and it did not answer before any fault was "+
			"injected: %v. A declared endpoint nothing can reach is the same gap as no endpoint, and "+
			"the scraper an operator runs here would record the same thing", err)
	}
	report.Before = before
	if before.Len() == 0 {
		t.Fatalf("the metrics endpoint answered and published no series: %s. Nothing was restarted yet, "+
			"so this is what an operator's scraper collects from this deployment at rest", before.Describe())
	}
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

	after, afterPod, resumed := waitMetricsBack(ctx, t, f, preFault, within)
	var scraped *framework.MetricSet
	if resumed {
		scraped = &after
		t.Logf("after the restart, %s: %s", afterPod, after.Describe())
	}
	report = framework.ClassifyMetrics(&before, scraped)
	t.Logf("%s", report)

	switch report.Verdict {
	case framework.MetricsNeverResumed:
		if !resumed {
			t.Fatalf("the metrics endpoint published %d series before the restart and nothing answered "+
				"at it within %s afterwards, while a replacement server pod is ready. Every dashboard "+
				"and every alert built on those series goes dark at the moment it is needed, which is "+
				"the failover. The endpoint was %s",
				before.Len(), within, endpoint)
		}
		t.Errorf("the server came back publishing %d of the %d metric families it published before the "+
			"restart, and %d are gone: %v. A family that stops being published across a failover takes "+
			"every rule written on it with it, and an operator watching this deployment sees the series "+
			"end rather than the server recover. Scraped from %s. This is what the test plan requires of "+
			"a deployment (Section 3.5), not an NFS protocol guarantee",
			len(after.Families()), len(before.Families()), len(report.LostFamilies), report.LostFamilies,
			afterPod)
	case framework.MetricsResumedReset:
		t.Logf("the endpoint came back with %d counters reset and %d carried across, which is what a "+
			"restarted process is supposed to look like and what every monitoring system knows how to "+
			"read", len(report.Reset), len(report.Continued))
	case framework.MetricsResumedContinuous:
		t.Logf("the endpoint came back with all %d comparable counters at or above where they were, so "+
			"this server keeps its counts across a restart", len(report.Continued))
	default:
		t.Errorf("the comparison produced the verdict %q, which this case has no reading for", report.Verdict)
	}

	// Diagnostics, deliberately not assertions. More families than before is a
	// server publishing more, and a label set that changed is a client or an
	// export that has not come back yet, neither of which is a defect.
	if len(report.NewFamilies) > 0 {
		t.Logf("%d families appeared only after the restart: %v", len(report.NewFamilies), report.NewFamilies)
	}
	if len(report.LostSeries) > 0 {
		t.Logf("%d series changed their labels across the restart while their family survived, which is "+
			"what a per-client or per-export label does when the clients reconnect: %v",
			len(report.LostSeries), report.LostSeries)
	}
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
func waitMetricsBack(ctx context.Context, t *testing.T, f *framework.Framework, preFault map[types.UID]bool,
	within time.Duration) (framework.MetricSet, string, bool) {
	t.Helper()
	var set framework.MetricSet
	var pod string
	var refused error
	err := framework.Poll(ctx, framework.PollInterval, within, func(ctx context.Context) (bool, error) {
		pods, err := framework.ServerPods(ctx, f.C)
		if err != nil {
			return false, err
		}
		for i := range pods {
			p := &pods[i]
			if preFault[p.UID] || !framework.PodReady(p) {
				continue
			}

			e, ok := framework.MetricsEndpointOf(p)
			if !ok {
				return false, fmt.Errorf("the replacement %s declares no metrics endpoint: %s",
					p.Name, framework.DescribePodPorts(p))
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
				return false, err
			}
			if m.Len() == 0 {
				return false, fmt.Errorf("%s", m.Describe())
			}
			set, pod = m, p.Name
			return true, nil
		}
		return false, fmt.Errorf("no replacement server pod is ready to scrape yet")
	})
	if refused != nil {
		blocked(t, "%v", refused)
	}
	if err != nil {
		t.Logf("the metrics endpoint did not answer within %s of the restart: %v", within, err)
		return framework.MetricSet{}, "", false
	}
	return set, pod, true
}
