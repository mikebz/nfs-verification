package e2e

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/framework"
	"github.com/mikebz/nfs-verification/pkg/slo"
)

// The identity the server records is the one the writing pod ran as. NFSv4
// carries owners as strings and maps them back through an idmapper on each
// client, so an id can survive on one node and read back as nobody on another.
// That is the failure this case exists to catch, and it is why ownership is
// read from two pods rather than one.

// testUID and testGID are arbitrary, and deliberately not a uid the image
// already knows: a name the container has never heard of is what exercises the
// numeric path rather than a lucky name match on both ends.
const (
	testUID = 1234
	testGID = 1234
	// nobody is the id an NFSv4 client substitutes when it cannot map an owner
	// string. Seeing it is the classic symptom, so it is named in the failure.
	nobodyUID = 65534
)

// SEC-01: uid and gid preservation across pods. A pod running as an ordinary
// user writes a file; another pod on another node reads the ownership back.
//
// Steps:
//  1. Pin a writer running as uid 1234 to one node, and a root reader to
//     another, on one claim.
//  2. From the reader, make a directory the ordinary user can write to. If
//     root cannot, report blocked: that is export configuration.
//  3. Write a file there as uid 1234. If that is refused, report blocked.
//  4. Read ownership on the writer's own client: a wrong id here means the
//     mapping broke on the way in, and no reader will fix it.
//  5. Read it again on the second client, and separate the two failures: wrong
//     on both is the server, wrong only on the reader is that node's idmapper.
func TestSecOwnershipPreservedAcrossPods(t *testing.T) {
	f := framework.New(t, "SEC-01")
	requireCap(t, f.Caps.MultiNode, "reading ownership from a second client needs two schedulable workers")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "sec01")
	// The writer runs as an ordinary user; the reader stays root so that it can
	// read ownership regardless of how the export treats the writer's id.
	f.MustPod(ctx, framework.PodSpec{
		Name: "writer", Node: nodeA, Mounts: mounts(pvc.Name),
		RunAsUser: framework.Int64(testUID), RunAsGroup: framework.Int64(testGID),
	})
	f.MustPod(ctx, toolsPod("reader", pvc.Name, nodeB))

	// The share's root belongs to whoever provisioned it, so the case makes a
	// directory the ordinary user can write to. Directory operations go to the
	// server, so the writer sees this without waiting on any cache.
	dir := fileIn("sec01")
	if r := f.Sh(ctx, "reader", "mkdir -p "+framework.Quote(dir)+" && chmod 0777 "+framework.Quote(dir)); r.Err != nil {
		t.Skipf("blocked on export configuration: root on %s cannot create a writable directory on the share (%s); "+
			"SEC-02 covers what the export does to root", nodeB, r.Combined())
	}
	// Best effort, and informative either way. A refusal here means root is
	// squashed, which is SEC-02's subject and does not stop this case: the
	// directory is already world-writable.
	if r := f.Sh(ctx, "reader", fmt.Sprintf("chown %d:%d %s", testUID, testGID, framework.Quote(dir))); r.Err != nil {
		t.Logf("root on %s could not chown on the share, so the export appears to squash root (%s); "+
			"this case continues through the world-writable directory", nodeB, r.Combined())
	}

	path := dir + "/owned.dat"
	if r := f.Sh(ctx, "writer", "echo sec01-payload > "+framework.Quote(path)); r.Err != nil {
		t.Skipf("blocked on export configuration: uid %d on %s cannot write to a 0777 directory on the share (%s); "+
			"the export is squashing or refusing ordinary users, which SEC-02 and SEC-05 cover",
			testUID, nodeA, r.Combined())
	}

	// The writer's own view first. If the id is already wrong here, the mapping
	// broke on the way in, and no reader is going to fix it.
	byWriter, err := f.StatOwner(ctx, "writer", path)
	if err != nil {
		t.Fatalf("reading ownership on %s: %v", nodeA, err)
	}
	if byWriter.UID != testUID || byWriter.GID != testGID {
		t.Errorf("the writer on %s wrote as %d:%d but its own client reports %s%s",
			nodeA, testUID, testGID, byWriter, nobodyNote(byWriter))
	}

	// Then the second client. A correct writer view with a wrong reader view is
	// a per-client idmapper problem, which routes to the node, not the server.
	byReader, err := f.StatOwner(ctx, "reader", path)
	if err != nil {
		t.Fatalf("reading ownership on %s: %v", nodeB, err)
	}
	switch {
	case byReader.UID == testUID && byReader.GID == testGID:
		t.Logf("ownership survived the crossing: %s on %s and %s on %s", byWriter, nodeA, byReader, nodeB)
	case byWriter.UID == testUID && byWriter.GID == testGID:
		t.Errorf("ownership is %s on the writing client %s but %s on the reading client %s%s: "+
			"the id survived the write and was lost on the read, which points at the reader's idmapper",
			byWriter, nodeA, byReader, nodeB, nobodyNote(byReader))
	default:
		t.Errorf("ownership is %s on %s, want %d:%d%s", byReader, nodeB, testUID, testGID, nobodyNote(byReader))
	}
}

// nobodyNote names the classic symptom when it is what happened, so that a
// failure report does not need a person to recognise 65534 on sight.
func nobodyNote(o framework.Owner) string {
	if o.UID == nobodyUID || o.GID == nobodyUID || o.User == "nobody" || o.Group == "nobody" {
		return " (mapped to nobody: the client could not resolve the owner string, " +
			"which is the reported failure on v4 clients with modern kernels)"
	}
	return ""
}

// SEC-02: who the server lets change a file's ownership.
//
// Two questions live here, and only one of them has a single correct answer.
//
// **Who may give a file away** is settled and the same everywhere: changing a
// file's owner requires privilege, and owning the file is not privilege
// ([`chown(2)`](https://man7.org/linux/man-pages/man2/chown.2.html)). An owner
// who could hand a file to somebody else could evade a quota, plant a file in
// another user's name, or launder what it wrote. A server that permits it is
// wrong however it is configured, so this half is asserted outright.
//
// **What happens to a client claiming uid 0** is a deployment choice. Under
// AUTH_SYS, the `sec=sys` this suite measures (plan Appendix B), nobody is
// authenticated: the client puts a uid in the request and the server takes its
// word for it. So root on any host that can reach the export can claim the
// server's root, and root squash is the server declining that claim by mapping
// uid 0 to an anonymous id. Most servers squash by default; a deployment whose
// workloads need root-owned files turns it off and accepts what that means.
// Neither is a defect, so what this case requires is that whichever rule is in
// force is applied **strictly**: the same answer on every client, with no
// half-squash. Where -root-squash states the intent, the behaviour must match
// it, and the mismatch is a failure.
//
// The probe is always the operation, never the number `stat` prints. NFSv4
// carries owners as strings, so a client whose idmapper cannot resolve one
// displays nobody over a file the server still owns as root. Reading 65534 and
// calling it squash is exactly how this case used to be wrong: it reported a
// client-side mapping as a server-side policy, and would have failed a
// correctly configured export the moment anyone passed -root-squash=off
// (F-021). Asking whether a privileged operation is permitted cannot be
// confused that way.
//
// Steps:
//  1. A root pod and an ordinary-uid pod on each of two nodes, on one claim.
//  2. Each ordinary user creates a file it owns and chmods it. The chmod must
//     succeed: it is the control, without which the refusal in step 3 could be
//     a server that refuses every metadata change rather than one enforcing a
//     privilege boundary.
//  3. Each ordinary user tries to chown its own file to a different uid. Both
//     must be refused.
//  4. Root tries to chown a file on each node. Whatever the answer, both nodes
//     must give it.
//  5. Compare against -root-squash where it was stated; otherwise record which
//     rule is in force and what it means on a shared cluster.
//  6. Record separately what the ownership reads back as, naming the idmapper
//     when that is what the display reflects.
func TestSecOwnershipChangePrivilege(t *testing.T) {
	f := framework.New(t, "SEC-02")
	requireCap(t, f.Caps.MultiNode, "requiring one rule on every client needs two schedulable workers")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "sec02")
	const rootA, rootB, userA, userB = "roota", "rootb", "usera", "userb"
	f.MustPod(ctx, toolsPod(rootA, pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod(rootB, pvc.Name, nodeB))
	for _, u := range []struct{ pod, node string }{{userA, nodeA}, {userB, nodeB}} {
		f.MustPod(ctx, framework.PodSpec{
			Name: u.pod, Node: u.node, Mounts: mounts(pvc.Name),
			RunAsUser: framework.Int64(testUID), RunAsGroup: framework.Int64(testGID),
		})
	}

	// World-writable, so an ordinary uid has somewhere to create the file it
	// owns whatever the share's own root permits. What is under test is who may
	// change ownership, not who may create a file.
	dir := fileIn("sec02")
	if r := f.Sh(ctx, rootA, "mkdir -p "+framework.Quote(dir)+" && chmod 0777 "+framework.Quote(dir)); r.Err != nil {
		blocked(t, "root on %s cannot create a writable directory on the share (%s), so there is nowhere "+
			"for an ordinary user to own a file and nothing to ask about ownership", nodeA, r.Combined())
	}

	// The rule that holds everywhere, asked on both clients.
	for _, u := range []struct{ pod, node string }{{userA, nodeA}, {userB, nodeB}} {
		own := fmt.Sprintf("%s/owned-by-%s.dat", dir, u.pod)
		if r := f.Sh(ctx, u.pod, "echo sec02 > "+framework.Quote(own)); r.Err != nil {
			blocked(t, "uid %d on %s cannot write to a 0777 directory on the share (%s), so it cannot own "+
				"the file this case asks about", testUID, u.node, r.Combined())
		}
		// The control. An owner may change its own file's mode, so a server
		// that refuses this refuses every metadata change and the refusal
		// below would prove nothing about privilege.
		if r := f.Sh(ctx, u.pod, "chmod 0640 "+framework.Quote(own)+" 2>&1"); r.Err != nil {
			blocked(t, "uid %d on %s cannot chmod a file it owns (%s). This server refuses metadata changes "+
				"outright, so a refusal to chown would say nothing about whether it enforces privilege",
				testUID, u.node, r.Combined())
		}

		allowed, out := chownAllowed(ctx, f, u.pod, own, testUID+1)
		if allowed {
			after, err := f.StatOwner(ctx, u.pod, own)
			detail := ""
			if err == nil {
				detail = fmt.Sprintf(" The file is now owned %s.", after)
			}
			t.Errorf("uid %d on %s owns %s and was allowed to give it away to uid %d.%s Changing a file's "+
				"owner requires privilege and owning the file is not privilege (chown(2)); a server that "+
				"lets any owner reassign a file lets one tenant plant files in another's name and evade "+
				"anything counted per owner", testUID, u.node, own, testUID+1, detail)
		} else {
			t.Logf("uid %d on %s was refused when it tried to give away a file it owns (%s)", testUID, u.node, out)
		}
	}

	// The rule that is a deployment choice, asked on both clients so that an
	// answer given inconsistently is caught rather than averaged.
	rootAllowed := make(map[string]bool, 2)
	rootSaid := make(map[string]string, 2)
	var asWritten framework.Owner
	var asWrittenErr error
	for _, r := range []struct{ pod, node string }{{rootA, nodeA}, {rootB, nodeB}} {
		own := fmt.Sprintf("%s/owned-by-%s.dat", dir, r.pod)
		if res := f.Sh(ctx, r.pod, "echo sec02 > "+framework.Quote(own)); res.Err != nil {
			t.Fatalf("root on %s cannot write to a 0777 directory on the share: %s", r.node, res.Combined())
		}
		if r.pod == rootA {
			// Read before the chown, or a permitted chown rewrites the very
			// ownership the record below is about.
			asWritten, asWrittenErr = f.StatOwner(ctx, r.pod, own)
		}
		rootAllowed[r.node], rootSaid[r.node] = chownAllowed(ctx, f, r.pod, own, testUID)
	}
	if rootAllowed[nodeA] != rootAllowed[nodeB] {
		t.Errorf("root's chown was %s on %s and %s on %s. The export applies one rule to uid 0 on one "+
			"client and the opposite on another, which is worse than either setting on its own: what a "+
			"workload may do to the shared volume then depends on where it was scheduled. %s said %q, %s "+
			"said %q", allowedWord(rootAllowed[nodeA]), nodeA, allowedWord(rootAllowed[nodeB]), nodeB,
			nodeA, rootSaid[nodeA], nodeB, rootSaid[nodeB])
	}

	squashed := !rootAllowed[nodeA]
	switch want := framework.Cfg().RootSquash; want {
	case "on":
		if !squashed {
			t.Errorf("-root-squash=on, but root on %s was allowed to change a file's owner, so the server "+
				"is honouring uid 0 rather than squashing it. Every pod in this cluster that can run as "+
				"root therefore has root on this share", nodeA)
		} else {
			t.Logf("root_squash is on as configured: root's chown was refused on both %s and %s", nodeA, nodeB)
		}
	case "off":
		if squashed {
			t.Errorf("-root-squash=off, but root on %s was refused a chown (%s), so the server is squashing "+
				"uid 0 when it was configured not to. A workload that needs to own files as root will fail "+
				"here in ways that look like permission bugs in the application", nodeA, rootSaid[nodeA])
		} else {
			t.Logf("root is preserved as configured: root's chown was permitted on both %s and %s", nodeA, nodeB)
		}
	default:
		// Recorded, not asserted. Nobody told this run what the export is meant
		// to do, and inventing an expectation would turn a deployment choice
		// into a test failure.
		if squashed {
			t.Logf("recorded: this export squashes root. Root's chown was refused on both %s and %s (%s). "+
				"Pass -root-squash=on to make that an assertion", nodeA, nodeB, rootSaid[nodeA])
		} else {
			t.Logf("recorded: this export does not squash root, which was allowed to change a file's owner "+
				"on both %s and %s. Under AUTH_SYS the server takes a client's word for its uid, so on a "+
				"shared cluster any pod that can run as root owns this share. Pass -root-squash=off to "+
				"make that an assertion", nodeA, nodeB)
		}
	}

	// What the ownership displays as, recorded and deliberately not used above.
	// This is the client's idmapper talking, and reading it as the server's
	// policy is the error F-021 records.
	if asWrittenErr != nil {
		t.Logf("reading back the ownership root's file displays as: %v", asWrittenErr)
		return
	}
	t.Logf("recorded: the file root wrote displays as %s on %s%s. That is what the client renders, not what "+
		"the server enforces, which is why the verdict above comes from the chown and not from this",
		asWritten, nodeA, nobodyNote(asWritten))
}

// chownAllowed reports whether a pod may change a file's owner, and returns
// what the attempt printed so that a refusal can be quoted.
//
// The attempt is the probe. Whether the server permits a privileged operation
// is the question; what `stat` displays afterwards is a rendering, and F-021 is
// what happens when the two are confused.
func chownAllowed(ctx context.Context, f *framework.Framework, pod, path string, uid int) (bool, string) {
	r := f.Sh(ctx, pod, fmt.Sprintf("chown %d %s 2>&1", uid, framework.Quote(path)))
	return r.Err == nil, strings.TrimSpace(r.Combined())
}

// allowedWord renders a permit/refuse outcome for a failure message.
func allowedWord(allowed bool) string {
	if allowed {
		return "permitted"
	}
	return "refused"
}

// testFSGroup is the gid SEC-03 declares. Like the uid above it is one nothing
// in the image knows, so nothing can pass by a lucky name match.
const testFSGroup = 5678

// SEC-03: whether a non-root pod can use an NFS volume by declaring fsGroup,
// and what declaring it costs everyone else on the share.
//
// Kubernetes promises a pod declaring fsGroup two things: the gid as a
// supplementary group, and a volume "modified to be owned and writable" by it.
// The second is conditional on the volume plugin supporting ownership
// management, which is a property of the plugin and not of the pod, so this
// case reports which of the two this deployment gives.
//
// Two assertions, pointing opposite ways, and both able to fail:
//
//   - Group access. A non-root pod declaring fsGroup must be able to read and
//     write the share. This is the pattern every restricted Pod Security
//     profile pushes workloads towards, so a deployment where it cannot write
//     is one where the standard pattern does not work.
//   - No chown storm. The ownership of files that were already there must be
//     unchanged. On a shared RWX volume a recursive chown is not a slowdown, it
//     is one tenant rewriting ownership another tenant relies on: exactly what
//     SEC-01 and SEC-02 assert stays put.
//
// The startup measurement is the second assertion's other half, not a
// performance test: a walk that rewrites nothing still costs it on every mount,
// so a share large enough turns one pod's fsGroup into a startup delay for
// every pod that declares it. It is measured against a control pod rather than
// stated absolutely because no absolute number is ratified anywhere, and
// because most of a pod's startup is scheduling and image pull, which have
// nothing to do with fsGroup. Both times come from the cluster, the creation
// stamp from the API server and the container start from the kubelet, never
// from the workstation (F-017).
//
// Steps:
//  1. Provision a claim and populate a directory on it from a root pod.
//  2. Count the ownership of everything under it.
//  3. Start a control pod, non-root, no fsGroup, and record how long it took to
//     become Ready.
//  4. Start an identical pod declaring fsGroup, on the same node, and record
//     the same interval.
//  5. Count the ownership again: anything rewritten is the storm.
//  6. Assert the startup difference is within the bound in pkg/slo.
//  7. Write from the fsGroup pod, and record which mechanism allowed it.
func TestSecFSGroupOnAnNFSVolume(t *testing.T) {
	f := framework.New(t, "SEC-03")
	ctx, cancel := caseCtx(t, 25*time.Minute)
	defer cancel()

	nodes, err := f.WorkerNodes(ctx)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("listing worker nodes: %v", err)
	}
	// One node for every pod here. The question is what kubelet does to the
	// volume on mount, and spreading the pods would add a second kubelet's
	// scheduling to a measurement that is about the first one's work.
	node := nodes[0]
	pvc := f.MustRWXPVC(ctx, "sec03")
	f.MustPod(ctx, toolsPod("populator", pvc.Name, node))

	dir := fileIn("sec03")
	created, err := f.PopulateDir(ctx, "populator", dir, slo.FSGroupPopulatedEntries, 16, 15*time.Minute)
	if err != nil {
		t.Fatalf("populating %s with %d entries: %v", dir, slo.FSGroupPopulatedEntries, err)
	}
	if created != slo.FSGroupPopulatedEntries {
		t.Fatalf("populated %d of %d entries, so a walk over the rest would not be measurable",
			created, slo.FSGroupPopulatedEntries)
	}
	// World-writable, so that a pod which is not root has somewhere to write
	// whatever the export's own permissions are. What is being tested is
	// fsGroup, not the mode of a directory the provisioner created.
	if r := f.Sh(ctx, "populator", "chmod 0777 "+framework.Quote(dir)); r.Err != nil {
		t.Fatalf("making %s writable: %v: %s", dir, r.Err, r.Combined())
	}

	before, err := f.OwnerCensusOf(ctx, "populator", dir, "sec03")
	if err != nil {
		t.Fatalf("counting ownership before the fsGroup pod started: %v", err)
	}
	t.Logf("before: %s", before)

	control := f.MustPod(ctx, framework.PodSpec{
		Name: "control", Node: node, Mounts: mounts(pvc.Name),
		RunAsUser: framework.Int64(testUID), RunAsGroup: framework.Int64(testGID),
	})
	member := f.MustPod(ctx, framework.PodSpec{
		Name: "member", Node: node, Mounts: mounts(pvc.Name),
		RunAsUser: framework.Int64(testUID), RunAsGroup: framework.Int64(testGID),
		FSGroup: framework.Int64(testFSGroup),
	})
	controlStart, err := podStartInterval(ctx, f, "control")
	if err != nil {
		t.Fatalf("reading how long the control pod took to start: %v", err)
	}
	memberStart, err := podStartInterval(ctx, f, "member")
	if err != nil {
		t.Fatalf("reading how long the fsGroup pod took to start: %v", err)
	}
	t.Logf("%s started in %s with no fsGroup; %s started in %s with fsGroup %d, over %d entries",
		control.Name, controlStart.Round(time.Second), member.Name, memberStart.Round(time.Second),
		testFSGroup, created)

	// The storm, if there was one, is visible in the ownership rather than in
	// the clock, so this is checked first and reported in full.
	after, err := f.OwnerCensusOf(ctx, "populator", dir, "sec03")
	if err != nil {
		t.Fatalf("counting ownership after the fsGroup pod started: %v", err)
	}
	if err := f.WriteArtifact("owner-census.txt",
		[]byte("before\n"+before.Raw+"\nafter\n"+after.Raw)); err != nil {
		t.Logf("writing the ownership census: %v", err)
	}
	if !before.SameOwnership(after) {
		t.Errorf("a pod declaring fsGroup %d rewrote the ownership of files that were already on the "+
			"share: %s became %s. On a shared RWX volume that is destructive rather than slow: it "+
			"rewrites ownership another workload is relying on, and it erases what SEC-01 and SEC-02 "+
			"assert on", testFSGroup, before, after)
	} else {
		t.Logf("the ownership of all %d entries is unchanged, so nothing walked the volume: %s",
			after.Total, after)
	}
	if overhead := memberStart - controlStart; overhead > slo.FSGroupStartOverhead {
		t.Errorf("the pod declaring fsGroup took %s longer to start than the identical pod without it "+
			"(%s against %s) over %d entries, above the %s bound. That is the shape of a recursive "+
			"chown on mount, and it grows with the share",
			overhead.Round(time.Second), memberStart.Round(time.Second), controlStart.Round(time.Second),
			created, slo.FSGroupStartOverhead)
	}

	// The promise itself: a pod that declared a gid must be able to use the
	// volume. Everything above is about what it cost; this is about whether it
	// worked.
	groups := strings.TrimSpace(f.Sh(ctx, "member", "id").Combined())
	path := dir + "/fsgroup.dat"
	if r := f.Sh(ctx, "member", "echo sec03 > "+framework.Quote(path)); r.Err != nil {
		t.Errorf("a pod running as uid %d with fsGroup %d cannot write to the share (%s). Its identity "+
			"inside the pod is %q. fsGroup is what a restricted Pod Security profile leaves a workload "+
			"to get group access with, and on this deployment it does not grant it: what decides is the "+
			"mode and owner of the export, which no pod spec can change",
			testUID, testFSGroup, r.Combined(), groups)
		return
	}
	owner, err := f.StatOwner(ctx, "member", path)
	if err != nil {
		t.Fatalf("reading the ownership of what the fsGroup pod wrote: %v", err)
	}
	// Recorded, not asserted. Which gid a new file lands with is decided by the
	// directory's setgid bit and the export's own ownership, and both are the
	// deployment's choice rather than a promise anybody made.
	switch {
	case owner.GID == testFSGroup:
		t.Logf("the file the fsGroup pod wrote is owned %s, so fsGroup reached the volume: this plugin "+
			"manages ownership", owner)
	default:
		t.Logf("the file the fsGroup pod wrote is owned %s rather than by gid %d, and the volume was "+
			"not rewritten, so fsGroup is a supplementary group inside the pod and nothing more here "+
			"(pod identity %q). The write succeeded on the directory's own permissions. An operator "+
			"relying on fsGroup for group access on this deployment is relying on the export's mode",
			owner, testFSGroup, groups)
	}
}

// podStartInterval is how long a pod took to go from created to running, from
// the cluster's own clocks: the API server stamps the creation and the kubelet
// stamps the container start.
//
// Not measured on the workstation. A laptop that sleeps mid-run understates
// every interval the suite takes about itself while the cluster-side stamps
// stay correct, which is F-017, and a mount that chowns a volume is exactly the
// kind of slow thing a workstation would be asleep through.
func podStartInterval(ctx context.Context, f *framework.Framework, pod string) (time.Duration, error) {
	p, err := f.C.Kube.CoreV1().Pods(framework.Namespace).Get(ctx, f.Name(pod), metav1.GetOptions{})
	if err != nil {
		return 0, err
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Running == nil {
			continue
		}
		d := cs.State.Running.StartedAt.Time.Sub(p.CreationTimestamp.Time)
		if d < 0 {
			return 0, fmt.Errorf("pod %s reports a container that started %s before the pod was created, "+
				"so the two stamps are not comparable", p.Name, (-d).Round(time.Second))
		}
		return d, nil
	}
	return 0, fmt.Errorf("pod %s has no running container to have started", p.Name)
}

// SEC-09: whether the server is running with the privileges it asked for.
//
// The failure this is hunting is a specific and quiet one, and it reaches a
// client as I/O errors from a server that looks healthy. An admission policy, a
// runtime default or a restricted bounding set can each remove a capability a
// workload declared, and the result is not a pod that refuses to start: it is a
// server that starts, serves most things, and returns EPERM on the operations
// that needed the missing capability. A file-handle backend without
// CAP_DAC_READ_SEARCH cannot open_by_handle_at, so reads fail on exactly the
// paths a client has no way to distinguish from storage corruption. The pod
// spec says one thing and the process does another, and nothing reports the
// gap; this case is where the gap gets reported.
//
// The other direction is the same question asked the other way: a privileged
// container holds everything regardless of what the spec says, so its declared
// set means nothing and no admission policy constrains it.
//
// What the runtime grants beyond the declaration is recorded, not asserted. It
// is the platform's default set, identical on every conformant cluster, so a
// case that failed on it would fail everywhere and say nothing.
//
// Steps:
//  1. Find the server pods.
//  2. Read the declared capabilities from the pod spec.
//  3. Read the effective set of the container's PID 1.
//  4. Fail on a declared capability that is not there at runtime.
//  5. Fail if the container is privileged.
//  6. Record the effective set and how far it exceeds the declaration.
func TestSecServerCapabilities(t *testing.T) {
	f := framework.New(t, "SEC-09")
	ctx, cancel := caseCtx(t, 10*time.Minute)
	defer cancel()

	pod := serverPodOrBlock(ctx, t, f)
	declared := framework.DeclaredCapsOf(pod)
	caps, err := framework.ContainerCaps(ctx, f.C, pod)
	if err != nil {
		failOrBlock(t, err, "reading the capability set of the server in %s/%s", pod.Namespace, pod.Name)
	}
	effective := framework.CapNames(caps.Effective)
	t.Logf("%s/%s container %s declares add=%v drop=%v privileged=%v and holds %v",
		pod.Namespace, pod.Name, declared.Container, declared.Add, declared.Drop, declared.Privileged, effective)
	if err := f.WriteArtifact("server-capabilities.txt", []byte(fmt.Sprintf(
		"pod: %s/%s\ncontainer: %s\ndeclared add: %v\ndeclared drop: %v\nprivileged: %v\n"+
			"effective: %v\npermitted: %v\nbounding: %v\nambient: %v\n",
		pod.Namespace, pod.Name, declared.Container, declared.Add, declared.Drop, declared.Privileged,
		effective, framework.CapNames(caps.Permitted), framework.CapNames(caps.Bounding),
		framework.CapNames(caps.Ambient)))); err != nil {
		t.Logf("writing the capability record: %v", err)
	}

	for _, want := range declared.Add {
		if !framework.HasCap(caps.Effective, want) {
			t.Errorf("the server container declares %s and does not hold it at runtime (effective set "+
				"%v). Something between the spec and the process removed it: an admission policy, a "+
				"restricted bounding set, or a runtime default. The pod started anyway, so what an "+
				"operator will see is the operations needing that capability failing with EPERM against "+
				"a server that looks healthy", want, effective)
		}
	}
	if declared.Privileged {
		t.Errorf("the server container runs privileged, so it holds every capability the kernel has and " +
			"nothing the platform's admission policy says constrains it. The plan's requirement for this " +
			"case is the set it needs and no more, and privileged is the one configuration under which " +
			"that cannot be true")
	}

	// Recorded rather than asserted: what the runtime grants by default is the
	// platform's choice, not the server's.
	extra := make([]string, 0, len(effective))
	for _, have := range effective {
		if !containsCap(declared.Add, have) {
			extra = append(extra, have)
		}
	}
	sort.Strings(extra)
	if len(declared.Add) > 0 && len(extra) > 0 {
		t.Logf("recorded: the container asked for %v and holds %d more from the runtime's defaults (%v). "+
			"Dropping ALL and adding back what it needs is what would narrow that",
			declared.Add, len(extra), extra)
	}
	if len(declared.Add) == 0 && !declared.Privileged {
		t.Logf("recorded: the server declares no capabilities and runs on the runtime's defaults (%v)", effective)
	}
}

// containsCap reports whether a declared list carries a capability name, with
// or without the CAP_ prefix that the Kubernetes API drops and the kernel
// documentation keeps.
func containsCap(list []string, name string) bool {
	for _, c := range list {
		if strings.EqualFold(strings.TrimPrefix(strings.ToUpper(c), "CAP_"),
			strings.TrimPrefix(strings.ToUpper(name), "CAP_")) {
			return true
		}
	}
	return false
}

// serverPodOrBlock resolves the one server pod the remaining security cases read
// from, and blocks the case where there is none: both callers are asking a
// question that has no answer without it.
//
// Resolved live, never from the cached environment record: preflight named the
// pod that was there when it ran, and the chaos cases move them.
func serverPodOrBlock(ctx context.Context, t *testing.T, f *framework.Framework) *corev1.Pod {
	t.Helper()
	pods, err := framework.ServerPods(ctx, f.C)
	if err != nil {
		t.Fatalf("finding the NFS server pods: %v", err)
	}
	for i := range pods {
		if framework.PodReady(&pods[i]) {
			return &pods[i]
		}
	}
	blocked(t, "no ready NFS server pod was found among %d matched, so nothing can be read from the "+
		"server's side; -server-namespace and -server-selector name it where the heuristic cannot",
		len(pods))
	return nil
}

// nfsPort is where NFSv4 is served. The protocol's own number, not a tunable of
// this deployment, so it is a constant here rather than a flag.
const nfsPort = 2049

// SEC-04: two nodes on one export, and whether the server keeps their state
// apart when one of them goes away.
//
// The question "can the server tell two clients apart" only matters for what it
// changes, and there are exactly two things. One is access control, because a
// per-client rule can only name something that can be told apart; that is the
// upstream report this row comes from, and SEC-05 settles it end to end by
// having an uninvited client try the mount, which is a stronger answer than any
// inspection of addresses. The other is the one no other case covers, and it is
// this one: NFSv4.1 state is held per client, so the client identity is the
// blast radius of every recovery. When a client goes away the server discards
// that client's opens and locks. If two nodes are one client to it, the server
// discards both.
//
// That failure is the worst kind for an application. The surviving pod is not
// told anything: it still believes it holds a lock, the file it was protecting
// is now writable by anyone, and it will never retry, because from its point of
// view nothing happened (RFC 8881 Section 8.4.2 on what a server may discard,
// and Section 9 for what a lock is worth).
//
// Everything here is observed from clients. The locks are taken and questioned
// through pods, and the question "does the server still hold node A's lock" is a
// LOCKT sent from node A's own pod. Nothing reads the server.
//
// The removal is deliberately a graceful one, not a force delete. Waiting for
// node B's mount to go means node B stops being a client of this server at all,
// rather than merely losing a file descriptor, which is what SEC-07 does within
// one node. It is also the safe order F-001 requires, and the ordinary way a
// workload leaves a node.
//
// Steps:
//  1. One claim, one pod on each of two nodes.
//  2. Require byte-range locks to reach the server on both mounts.
//  3. Each pod takes a write lock on a disjoint range of one file.
//  4. Confirm the server holds both at once, each asked from the other node.
//  5. Delete node B's pod gracefully and wait until node B has no mount left.
//  6. Node B's range must be free, which is what proves state was discarded at
//     all, so that step 7 cannot pass by nothing having happened.
//  7. Node A's range must still be held, and node A must still be able to read
//     and write through its mount.
func TestSecServerKeepsTwoClientsStateApart(t *testing.T) {
	f := framework.New(t, "SEC-04")
	requireCap(t, f.Caps.MultiNode, "asking whether two clients' state is separable needs two schedulable workers")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "sec04")
	const stayer, leaver = "stayer", "leaver"
	f.MustPod(ctx, toolsPod(stayer, pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod(leaver, pvc.Name, nodeB))

	pv, err := f.PVForClaim(ctx, "sec04")
	if err != nil {
		t.Fatalf("finding the volume behind the claim: %v", err)
	}
	requireServerSideLocking(ctx, t, f, pv.Name, framework.PosixLock, nodeA, nodeB)

	path := fileIn("sec04.lock")
	// Disjoint, so neither lock can ever be refused because of the other and a
	// range that changes hands has done so for a reason to do with identity.
	stayerRange := framework.WriteRange(0, 4096)
	leaverRange := framework.WriteRange(4096, 4096)
	f.MustShf(ctx, stayer, "dd if=/dev/zero of=%s bs=4k count=2 conv=fsync 2>/dev/null", framework.Quote(path))

	kept, err := f.HoldLock(ctx, stayer, path, "sec04stay", stayerRange)
	if err != nil {
		failOrBlock(t, err, "taking %s in %s on %s", stayerRange, stayer, nodeA)
	}
	f.Defer(func(ctx context.Context) { _ = kept.Release(ctx) })
	if _, err := f.HoldLock(ctx, leaver, path, "sec04leave", leaverRange); err != nil {
		failOrBlock(t, err, "taking %s in %s on %s", leaverRange, leaver, nodeB)
	}
	// No Release registered for the leaver: its pod is about to be deleted, and
	// a cleanup that execs into a pod that is gone reports an error that means
	// nothing.

	// Each range asked from the other node, so both answers come from the
	// server rather than from the asking node's own kernel. A range the server
	// does not think is held would make everything below vacuous.
	for _, c := range []struct {
		asker string
		rng   framework.LockRange
		owner string
	}{{leaver, stayerRange, nodeA}, {stayer, leaverRange, nodeB}} {
		ans, err := f.GetLock(ctx, c.asker, path, c.rng)
		if err != nil {
			failOrBlock(t, err, "asking the server who holds %s", c.rng)
		}
		if ans.Free {
			t.Fatalf("the server reports %s free while a pod on %s holds it, so the locks are not "+
				"reaching the server and this case cannot say anything about whose state is whose",
				c.rng, c.owner)
		}
	}
	t.Logf("the server holds %s for a pod on %s and %s for a pod on %s at once, so it has state for "+
		"both clients", stayerRange, nodeA, leaverRange, nodeB)
	if err := f.RecordNodeLocks(ctx, "before-node-b-leaves", nodeA, nodeB); err != nil {
		t.Logf("recording the client lock tables before the fault: %v", err)
	}

	// Node B stops being a client: the pod goes gracefully and the mount with
	// it. A pod that will not leave the API means the node stopped answering,
	// which is a hazard rather than a slow unmount to wait out (F-001).
	if err := f.DeletePod(ctx, leaver); err != nil {
		t.Fatalf("deleting %s on %s: %v", leaver, nodeB, err)
	}
	if err := f.WaitPodGone(ctx, leaver, framework.UnmountTimeout); err != nil {
		t.Fatalf("%s did not leave the API after a graceful delete: %v. That means %s stopped answering "+
			"rather than that the unmount is slow, and this case stops here rather than waiting on a "+
			"node in that state", leaver, err, nodeB)
	}
	if err := f.AwaitUnmount(ctx, nodeB, pv, framework.UnmountTimeout); err != nil {
		t.Fatalf("%s still has %s mounted after its only pod went: %v. Node B has not stopped being a "+
			"client, so what follows would not be the question this case asks", nodeB, pv.Name, err)
	}
	t.Logf("%s has released the volume, so it is no longer a client of this server", nodeB)
	if err := f.RecordNodeLocks(ctx, "after-node-b-leaves", nodeA, nodeB); err != nil {
		t.Logf("recording the client lock tables after the fault: %v", err)
	}

	// The control first. If the departed client's own range is still held,
	// nothing was discarded, and the survival below would be survival of a
	// fault that never landed.
	gone, err := f.GetLock(ctx, stayer, path, leaverRange)
	if err != nil {
		failOrBlock(t, err, "asking the server whether %s was released", leaverRange)
	}
	if !gone.Free {
		t.Errorf("%s was held by a pod on %s, which has gone and whose mount is released, and the server "+
			"still holds it: %s. Nothing was discarded, so this run says nothing about whether one "+
			"client's loss reaches another; DATA-06 owns the question of when a departed client's lock "+
			"comes back", leaverRange, nodeB, gone.Conflict)
	} else {
		t.Logf("%s was discarded when %s stopped being a client", leaverRange, nodeB)
	}

	// The assertion.
	held, err := f.GetLock(ctx, stayer, path, stayerRange)
	if err != nil {
		failOrBlock(t, err, "asking the server whether %s survived", stayerRange)
	}
	if held.Free {
		t.Errorf("a pod on %s held %s, and the server released it when the unrelated client on %s went "+
			"away. The two nodes are one client to this server, so one node losing its mount discards "+
			"another node's locks: the application on %s still believes it holds %s, the file it was "+
			"protecting is open to anyone, and it will never retry because nothing told it",
			nodeA, stayerRange, nodeB, nodeA, stayerRange)
	} else {
		t.Logf("%s is still held on %s after %s stopped being a client, and the server names %s as the "+
			"holder, so the two nodes' state is separate", stayerRange, nodeA, nodeB, held.Conflict)
	}

	// The application, not only the lock: a session the server tore down shows
	// up here as an I/O error rather than as a missing lock.
	if r := f.Sh(ctx, stayer, "dd if=/dev/zero of="+framework.Quote(path)+
		" bs=1k count=1 conv=notrunc,fsync 2>&1"); r.Err != nil {
		t.Errorf("%s on %s cannot write through its mount after an unrelated client on %s went away: "+
			"%v: %s. The departure took more than a lock with it", stayer, nodeA, nodeB, r.Err, r.Combined())
	}
	if r := f.Sh(ctx, stayer, "dd if="+framework.Quote(path)+" of=/dev/null bs=4k count=2 2>&1"); r.Err != nil {
		t.Errorf("%s on %s cannot read through its mount after an unrelated client on %s went away: "+
			"%v: %s", stayer, nodeA, nodeB, r.Err, r.Combined())
	}
}

// containsAddr reports whether an address is in a list, comparing the unmapped
// form so that an IPv4 client arriving on an IPv6 listener as ::ffff:a.b.c.d
// matches the plain address the Kubernetes API gave for its node.
func containsAddr(list []netip.Addr, want netip.Addr) bool {
	for _, a := range list {
		if a.Unmap() == want.Unmap() {
			return true
		}
	}
	return false
}

// SEC-05: a client the export was never asked to serve attempts to mount it.
//
// Everything the other security cases assert rests on this one. Ownership,
// squash and capabilities all describe what a client the export admits may do;
// they say nothing if anything that can reach 2049 may mount any export on the
// server and read it as root.
//
// The probe mounts outside kubelet, which is the one new hazard in this phase,
// and pkg/framework/probemount.go carries the safety case: never in the host
// mount namespace (F-003, F-005), soft rather than hard, bounded, and always
// unmounted.
//
// A refusal is only reported as the server's when the instrument is proven,
// because a container that cannot mount NFS at all and an export refusing a
// stranger both reach userspace as EACCES. The control is a probe of a second
// export, from the same node and the same container, that this node already has
// mounted through kubelet: it can only fail for a client-side reason.
//
// Steps:
//  1. Two claims: the owner's on node A, and a neighbour's on node B.
//  2. Write a file into the owner's claim and checksum it.
//  3. From node B's agent container, probe the neighbour's export. This is the
//     control, and a refusal here reports blocked.
//  4. From the same place, probe the owner's export, which node B has never
//     been served and has no claim on.
//  5. Granted is a failure, and the owner's checksum read through the probe is
//     the evidence. An attributable refusal is a pass. Anything else is
//     blocked.
func TestSecUninvitedClientMountsAnExport(t *testing.T) {
	f := framework.New(t, "SEC-05")
	requireCap(t, f.Caps.MultiNode, "the probe needs a node this export has never been served, alongside the one it has")
	requireCap(t, f.Caps.NodeAgent, "attempting a mount outside kubelet needs the privileged node agent")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	ownerPVC := f.MustRWXPVC(ctx, "sec05owner")
	f.MustPod(ctx, toolsPod("owner", ownerPVC.Name, nodeA))
	// A second claim, mounted on the node the probe runs from. The server has
	// already served this node that export, so a probe of it from there can
	// only fail for a reason on the client's side, which is what makes it a
	// control rather than a second unknown.
	nearPVC := f.MustRWXPVC(ctx, "sec05near")
	f.MustPod(ctx, toolsPod("neighbour", nearPVC.Name, nodeB))

	// Random bytes, so a matching checksum cannot be a coincidence of two empty
	// files or of a filesystem that returns zeroes for anything.
	const relPath = "sec05-owner.dat"
	path := fileIn(relPath)
	f.MustShf(ctx, "owner", "dd if=/dev/urandom of=%s bs=4k count=16 conv=fsync 2>/dev/null",
		framework.Quote(path))
	sum, err := f.Sha256(ctx, "owner", path)
	if err != nil {
		t.Fatalf("checksumming the owner's file on %s: %v", nodeA, err)
	}
	t.Logf("the owner on %s wrote %s with checksum %s", nodeA, path, sum)

	ownerSrc := sourceOrBlock(ctx, t, f, "sec05owner")
	nearSrc := sourceOrBlock(ctx, t, f, "sec05near")

	control, err := f.ProbeMount(ctx, nodeB, nearSrc, "", "sec05ctl")
	if err != nil {
		failOrBlock(t, err, "the control mount of %s from %s", nearSrc, nodeB)
	}
	if !control.Granted {
		blocked(t, "the control mount of %s from %s was refused (%s). That export is already mounted on "+
			"this node by kubelet, so the refusal is the instrument's and not the server's, and a "+
			"refusal of the export under test could not be attributed to anybody. An unproven probe "+
			"reports blocked rather than a pass", nearSrc, nodeB, control.MountMessage)
	}
	t.Logf("the control mount of %s from %s was granted, so this container can mount NFS and a refusal "+
		"below is the server's", nearSrc, nodeB)

	probe, err := f.ProbeMount(ctx, nodeB, ownerSrc, relPath, "sec05")
	if err != nil {
		failOrBlock(t, err, "probing %s from %s, a node it has never been served", ownerSrc, nodeB)
	}
	if err := f.WriteArtifact("mount-probe.txt", []byte(fmt.Sprintf(
		"owner export: %s (claim mounted on %s)\nprobe node: %s\nowner checksum: %s\n\n"+
			"control probe of %s:\n%s\n\nprobe of the owner's export:\n%s\n",
		ownerSrc, nodeA, nodeB, sum, nearSrc, control.Output, probe.Output))); err != nil {
		t.Logf("writing the probe record: %v", err)
	}

	switch {
	case probe.Granted:
		evidence := fmt.Sprintf("it read the owner's file back as %s", shortened(probe.Sum))
		if probe.Sum == "" {
			evidence = "it could not read the owner's file, so what it reached is the export and not " +
				"necessarily these bytes"
		} else if probe.Sum != sum {
			evidence = fmt.Sprintf("it read %s where the owner wrote %s, so the mount reached the "+
				"server but not this file", shortened(probe.Sum), shortened(sum))
		}
		t.Errorf("a client with no claim on it mounted %s from %s, a node this export was never "+
			"provisioned for, and %s. The export admits any client that can route to %s, so every "+
			"claim on this server is readable and writable by anything in the cluster that can reach "+
			"it: the Kubernetes access controls around a claim end at the mount, and nothing below "+
			"them is enforcing anything. Probe output: %s",
			ownerSrc, nodeB, evidence, ownerSrc.Server, framework.Quote(probe.Output))
	case probe.DeniedByServer():
		t.Logf("the export refused a client it was never asked to serve: %s. The control from the same "+
			"container was granted, so this is the server's answer and not the probe's", probe.MountMessage)
	default:
		blocked(t, "the probe of %s from %s neither mounted nor produced a refusal this harness can "+
			"attribute to the server (%s). The control was granted, so the container can mount; what "+
			"this outcome is, is unknown, and an unattributable refusal is not a pass",
			ownerSrc, nodeB, probe.MountMessage)
	}
}

// sourceOrBlock resolves the export behind a claim, or reports blocked.
//
// A volume whose export this harness cannot read is a fact about the driver's
// volumeAttributes, not a defect in the storage: the extraction says which key
// it looked for, and the case cannot run until somebody adds it.
func sourceOrBlock(ctx context.Context, t *testing.T, f *framework.Framework, claim string) framework.NFSSource {
	t.Helper()
	pv, err := f.PVForClaim(ctx, claim)
	if err != nil {
		t.Fatalf("finding the volume behind claim %s: %v", claim, err)
	}
	src, err := framework.ExtractNFSSource(pv)
	if err != nil {
		blocked(t, "%v", err)
	}
	return src
}

// shortened keeps a checksum quotable in a failure message.
func shortened(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	if sum == "" {
		return "none"
	}
	return sum
}

// SEC-06: one client, two address families.
//
// A server that normalises addresses inconsistently has two identities for one
// client, and the consequence is an export rule that matches over one family
// and not the other: a node that mounts over IPv4 today and IPv6 after a
// restart gets a different answer from the same rule. The IPv4-mapped form
// (::ffff:a.b.c.d) is where this goes wrong, because a server listening on IPv6
// sees every IPv4 client in it and a rule written as a plain IPv4 address may
// or may not be compared against it.
//
// Both mounts deliberately bypass the Service and address the server pod
// directly, one address per family. The Service is a single-family object on
// most clusters, so going through it could only ever exercise one side, and
// what is under test is the server's view of one client rather than the proxy's.
//
// Steps:
//  1. Provision a claim on one node, so an export exists that this node is
//     already a client of.
//  2. Read the server pod's own addresses; block unless it has both families.
//  3. Clone the export twice, once per family, and mount both on that node.
//  4. Write through each, so each has an established connection.
//  5. Require the peer the server records for each to be that node's own
//     address in that family. Anything else is one client seen as two.
func TestSecClientIdentityAcrossAddressFamilies(t *testing.T) {
	f := framework.New(t, "SEC-06")
	requireCap(t, f.Caps.DualStack, "mounting one export over both address families needs a dual-stack cluster")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	nodes, err := f.WorkerNodes(ctx)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("listing worker nodes: %v", err)
	}
	// One node throughout. Two families from one client is the subject; two
	// clients is SEC-04's.
	node := nodes[0]
	pvc := f.MustRWXPVC(ctx, "sec06")
	f.MustPod(ctx, toolsPod("owner", pvc.Name, node))
	src := sourceOrBlock(ctx, t, f, "sec06")

	server := serverPodOrBlock(ctx, t, f)
	byFamily := addrsByFamily(server.Status.PodIPs)
	if len(byFamily) < 2 {
		blocked(t, "the cluster reports both address families but server pod %s/%s has only %v, so the "+
			"same export cannot be mounted over each and there is no second identity to compare. That "+
			"is a fact about how this server is deployed, not about its address handling",
			server.Namespace, server.Name, server.Status.PodIPs)
	}
	nodeAddrs, err := framework.NodeAddresses(ctx, f.C, node)
	if err != nil {
		t.Fatalf("reading the addresses of %s: %v", node, err)
	}

	// Mounted before anything is read, both of them, so that the peer table is
	// a single observation of one client holding two connections rather than
	// two observations that could each have been a different client.
	type mount struct {
		family string
		claim  string
	}
	var made []mount
	for _, fam := range []string{"v4", "v6"} {
		addr, ok := byFamily[fam]
		if !ok {
			continue
		}
		host := addr.String()
		if addr.Is6() {
			// mount(8) needs the brackets to tell the address from the colon
			// that separates it from the export path.
			host = "[" + host + "]"
		}
		claim := "sec06" + fam
		if _, _, err := f.CloneVolume(ctx, framework.CloneVolumeSpec{
			Name: claim, Source: framework.NFSSource{Server: host, Path: src.Path},
			Options: []string{"vers=4.1", "hard"},
		}); err != nil {
			t.Fatalf("cloning %s over the server's %s address %s: %v", src, fam, host, err)
		}
		f.MustPod(ctx, toolsPod("client"+fam, claim, node))
		path := fileIn("sec06-" + fam + ".dat")
		if r := f.Sh(ctx, "client"+fam, "dd if=/dev/zero of="+framework.Quote(path)+
			" bs=4k count=4 conv=fsync 2>&1"); r.Err != nil {
			t.Fatalf("writing through the %s mount on %s: %v: %s", fam, node, r.Err, r.Combined())
		}
		made = append(made, mount{family: fam, claim: claim})
	}

	conns, err := framework.ServerConns(ctx, f.C, server)
	if err != nil {
		failOrBlock(t, err, "reading the socket table of server pod %s/%s", server.Namespace, server.Name)
	}
	var record strings.Builder
	fmt.Fprintf(&record, "node: %s with addresses %v\nserver pod addresses: %v\n\nsocket table:\n",
		node, nodeAddrs, server.Status.PodIPs)
	for _, c := range conns {
		fmt.Fprintf(&record, "  %s\n", c)
	}
	if err := f.WriteArtifact("dual-stack-peers.txt", []byte(record.String())); err != nil {
		t.Logf("writing the peer table: %v", err)
	}

	for _, m := range made {
		want, ok := familyAddr(nodeAddrs, m.family)
		if !ok {
			t.Errorf("%s mounted the export over %s and the Kubernetes API gives it no %s address (%v), "+
				"so there is no identity an export rule could be written against for that family",
				node, m.family, m.family, nodeAddrs)
			continue
		}
		peers := peersOfFamily(framework.PeersOn(conns, nfsPort), m.family)
		if !containsAddr(peers, want) {
			t.Errorf("the client mounting over %s is %s, and the server records no connection from it: "+
				"the %s peers it does see are %v. One client reaching a server over two families must "+
				"be one identity in each, or a per-client rule matches over one family and not the "+
				"other, and which one a node gets is decided by a mount it did not choose",
				m.family, want, m.family, peers)
			continue
		}
		t.Logf("the server records %s as %s over %s, which is that node's own address in that family",
			node, want, m.family)
	}

	// Recorded rather than asserted: whether an IPv4 client on an IPv6 listener
	// arrives mapped is the server's socket setup, and the fact an operator
	// needs is that a rule may have to be written in the mapped form.
	for _, c := range conns {
		if c.Mapped && c.Local.Port() == nfsPort {
			t.Logf("recorded: %s. An IPv4 client reaches this server's IPv6 listener in the mapped "+
				"form, so an export rule written as a plain IPv4 address is only matched if the "+
				"server normalises it", c)
			break
		}
	}
}

// addrsByFamily indexes a pod's addresses by family, keeping the first of each.
func addrsByFamily(ips []corev1.PodIP) map[string]netip.Addr {
	out := map[string]netip.Addr{}
	for _, ip := range ips {
		addr, err := netip.ParseAddr(ip.IP)
		if err != nil {
			continue
		}
		key := familyOf(addr)
		if _, seen := out[key]; !seen {
			out[key] = addr
		}
	}
	return out
}

// familyOf names an address's family. The unmapped form decides it: an
// IPv4-mapped address is an IPv4 client however the server wrote it down.
func familyOf(addr netip.Addr) string {
	if addr.Unmap().Is4() {
		return "v4"
	}
	return "v6"
}

// familyAddr returns the first address of a family in a list.
func familyAddr(addrs []netip.Addr, family string) (netip.Addr, bool) {
	for _, a := range addrs {
		if familyOf(a) == family {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// peersOfFamily narrows a peer list to one family.
func peersOfFamily(peers []netip.Addr, family string) []netip.Addr {
	var out []netip.Addr
	for _, a := range peers {
		if familyOf(a) == family {
			out = append(out, a)
		}
	}
	return out
}

// SEC-07: two pods on one node, which is one client to the server, and one of
// them vanishes.
//
// A Linux NFS client establishes one lease per server and shares it across
// every mount and every pod on that node
// (https://docs.kernel.org/filesystems/nfs/client-identifier.html). Two pods on
// one node are therefore indistinguishable to the server, and the failure that
// makes possible is a state collision: one pod's departure taking another pod's
// locks with it, because the server has no way to tell whose they were.
//
// The vanished pod is modelled by a force delete, which F-001 blesses for this
// case and DATA-06 and nothing else. Unlike DATA-06 this case does not wait for
// the node to release the mount, and must not: the survivor still has the same
// claim mounted, legitimately, so the mount is supposed to remain. What keeps
// teardown safe is the survivor itself, which is deleted gracefully and waited
// for in the ordinary way before any claim is touched.
//
// The lock state is read from a third pod on the other node. A prober on the
// same node would be asking its own kernel, which knows the ranges locally; a
// prober on another node is a different client, and its LOCKT is answered by
// the server. What the server believes is the whole question here.
//
// Steps:
//  1. Two pods on node A on one claim, and a prober on node B.
//  2. Require byte-range locks to reach the server on both mounts.
//  3. Each node A pod takes a write lock on a disjoint range of one file.
//  4. Confirm from the prober that the server holds both.
//  5. Force-delete one of them.
//  6. Wait for its range to become free, within the lease bound.
//  7. The survivor's range must still be held, and the survivor must still be
//     able to write through its mount.
//  8. A replacement pod on the same node must be granted the freed range, and
//     the survivor's lock must still be held after it is.
func TestSecSharedClientIdentityAfterPodLoss(t *testing.T) {
	f := framework.New(t, "SEC-07")
	requireCap(t, f.Caps.MultiNode, "reading what the server believes needs a second client, which means a second node")
	ctx, cancel := caseCtx(t, 20*time.Minute)
	defer cancel()

	nodeA, nodeB := f.TwoNodes(ctx)
	pvc := f.MustRWXPVC(ctx, "sec07")
	const survivor, vanisher, prober, replacement = "survivor", "vanisher", "prober", "replacement"
	f.MustPod(ctx, toolsPod(survivor, pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod(vanisher, pvc.Name, nodeA))
	f.MustPod(ctx, toolsPod(prober, pvc.Name, nodeB))

	pv, err := f.PVForClaim(ctx, "sec07")
	if err != nil {
		t.Fatalf("finding the volume behind the claim: %v", err)
	}
	requireServerSideLocking(ctx, t, f, pv.Name, framework.PosixLock, nodeA, nodeB)

	path := fileIn("sec07.lock")
	// Disjoint ranges of one file, so that neither lock could ever be refused
	// because of the other, and a range that changes hands has done so for a
	// reason to do with client identity.
	survivorRange := framework.WriteRange(0, 4096)
	vanisherRange := framework.WriteRange(4096, 4096)
	f.MustShf(ctx, survivor, "dd if=/dev/zero of=%s bs=4k count=2 conv=fsync 2>/dev/null",
		framework.Quote(path))

	kept, err := f.HoldLock(ctx, survivor, path, "sec07keep", survivorRange)
	if err != nil {
		failOrBlock(t, err, "taking %s in %s on %s", survivorRange, survivor, nodeA)
	}
	f.Defer(func(ctx context.Context) { _ = kept.Release(ctx) })
	if _, err := f.HoldLock(ctx, vanisher, path, "sec07gone", vanisherRange); err != nil {
		failOrBlock(t, err, "taking %s in %s on %s", vanisherRange, vanisher, nodeA)
	}
	// No Release registered for the second holder on purpose: the pod it runs
	// in is about to be force-deleted, and a cleanup that execs into a pod that
	// is gone reports an error that means nothing.

	// Both, from the other node, before the fault. A range the server does not
	// think is held would make everything below vacuous.
	for _, r := range []framework.LockRange{survivorRange, vanisherRange} {
		ans, err := f.GetLock(ctx, prober, path, r)
		if err != nil {
			failOrBlock(t, err, "asking the server who holds %s", r)
		}
		if ans.Free {
			t.Fatalf("the server reports %s free while a pod on %s holds it. Two pods on one node share "+
				"one client identity, and this case cannot say anything about that if the locks never "+
				"reached the server in the first place", r, nodeA)
		}
	}
	t.Logf("the server holds %s for %s and %s for %s, both on %s and so both under one client identity",
		survivorRange, survivor, vanisherRange, vanisher, nodeA)
	// Both clients' own tables, before the fault, because the interesting
	// failure is a disagreement between them and the server afterwards and
	// there is nothing to compare against without this.
	if err := f.RecordNodeLocks(ctx, "before-force-delete", nodeA, nodeB); err != nil {
		t.Logf("recording the client lock tables before the fault: %v", err)
	}

	// Force delete, and no unmount wait: the survivor has the same claim
	// mounted on this node and is entitled to keep it. See F-001 for why every
	// other path in this suite waits.
	if err := f.DeletePodNow(ctx, vanisher); err != nil {
		t.Fatalf("force-deleting %s on %s: %v", vanisher, nodeA, err)
	}
	deletedAt := time.Now()

	bound := slo.LockReleaseBound(profile(t))
	stillHeld := framework.Poll(ctx, framework.PollInterval, bound+slo.ObservationMargin,
		func(ctx context.Context) (bool, error) {
			ans, err := f.GetLock(ctx, prober, path, vanisherRange)
			if err != nil {
				return false, err
			}
			return ans.Free, fmt.Errorf("%s is still held by %s", vanisherRange, ans.Conflict)
		})
	released := time.Since(deletedAt)
	if err := f.RecordNodeLocks(ctx, "after-force-delete", nodeA, nodeB); err != nil {
		t.Logf("recording the client lock tables after the fault: %v", err)
	}

	// The survivor first, whatever happened to the other range: that is this
	// case's question, and it has an answer either way.
	held, err := f.GetLock(ctx, prober, path, survivorRange)
	if err != nil {
		failOrBlock(t, err, "asking the server whether %s survived", survivorRange)
	}
	if held.Free {
		t.Errorf("%s on %s held %s, and losing %s on the same node released it. The two pods are one "+
			"client to the server, which is how a Linux client works, so a pod that vanishes can take "+
			"another pod's state with it: the surviving application still believes it holds a lock "+
			"nothing is protecting, which is worse than losing it, because it will not retry",
			survivor, nodeA, survivorRange, vanisher)
	} else {
		t.Logf("%s is still held after the pod sharing its client identity was force-deleted, and the "+
			"server names %s as the holder", survivorRange, held.Conflict)
	}
	// The application, not only the lock: a session the server tore down would
	// show here as an I/O error rather than as a missing lock.
	if r := f.Sh(ctx, survivor, "dd if=/dev/zero of="+framework.Quote(path)+
		" bs=1k count=1 conv=notrunc,fsync 2>&1"); r.Err != nil {
		t.Errorf("%s on %s cannot write through its mount after a pod sharing its client identity was "+
			"force-deleted: %v: %s. The departure took more than a lock with it",
			survivor, nodeA, r.Err, r.Combined())
	}

	if stillHeld != nil {
		t.Errorf("%s was held by %s on %s, which was force-deleted %s ago, and the server still holds "+
			"it: %v. The bound is one lease on the %s profile, because the lease is the only thing "+
			"that releases state nothing closed. DATA-06 owns this assertion for a single pod; here it "+
			"also means the replacement below cannot be tested",
			vanisherRange, vanisher, nodeA, released.Round(time.Second), stillHeld, profile(t).Name)
		return
	}
	t.Logf("%s became free %s after its holder was force-deleted", vanisherRange, released.Round(time.Second))

	// The other half of the shared identity: a new pod on the same node is the
	// same client to the server, and a server that tied the released state to
	// the client rather than to the departed process would refuse it.
	f.MustPod(ctx, toolsPod(replacement, pvc.Name, nodeA))
	taken, err := f.HoldLock(ctx, replacement, path, "sec07new", vanisherRange)
	if err != nil {
		failOrBlock(t, err, "taking %s in a replacement pod on %s, the node whose client identity the "+
			"force-deleted pod shared", vanisherRange, nodeA)
	}
	f.Defer(func(ctx context.Context) { _ = taken.Release(ctx) })
	t.Logf("a replacement pod on %s was granted %s, so the client identity the vanished pod shared is "+
		"usable again", nodeA, vanisherRange)

	after, err := f.GetLock(ctx, prober, path, survivorRange)
	if err != nil {
		failOrBlock(t, err, "asking the server whether %s survived the replacement", survivorRange)
	}
	if after.Free {
		t.Errorf("%s was still held after the force delete and is free once a replacement pod on %s "+
			"took %s. A new pod reusing the node's client identity has reclaimed state belonging to a "+
			"pod that never went away", survivorRange, nodeA, vanisherRange)
	}
}

// dataPathProbeTimeout bounds the one connection attempt SEC-08 makes. Short,
// because the answer it wants is whether the port is reachable at all, and a
// pod that cannot reach it should say so rather than sit in a retry.
const dataPathProbeTimeout = 5 * time.Second

// rpcTLSPort is the port RFC 9289 assigns to RPC-over-TLS. A server offering it
// is a server where a confidential mount is possible at all.
const rpcTLSPort = 20049

// SEC-08: the data path, stated rather than judged.
//
// The plan makes this row a finding, not a verdict, and this case does not
// quietly upgrade it. AUTH_SYS over cleartext TCP is what almost every NFS
// deployment runs, and failing a deployment for it would fail every deployment
// this suite will ever meet. What an operator cannot get anywhere else is a
// statement of what is actually in force here, which is what this records.
//
// Four readings, none asserted, and the case fails only if it could take none
// of them, which is the one way a recording case can be wrong.
//
// No packet capture is taken. It would be the direct evidence, and it needs
// tcpdump on a node image that has none; F-006 is the precedent for what an
// unverified instrument is worth, and what it is worth is a blocked case rather
// than a confident sentence about bytes nobody looked at.
//
// Steps:
//  1. Mount a claim and read the mount's own options: the security flavour,
//     the transport, and whether any transport security appears at all.
//  2. Read the server's listening ports, to say whether a secured port is
//     offered.
//  3. From a pod with no claim, attempt a connection to the server's port.
//  4. List the NetworkPolicies in the server's namespace.
//  5. Write all four into the bundle, and fail only if none could be read.
func TestSecDataPathConfidentiality(t *testing.T) {
	f := framework.New(t, "SEC-08")
	ctx, cancel := caseCtx(t, 15*time.Minute)
	defer cancel()

	nodes, err := f.WorkerNodes(ctx)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("listing worker nodes: %v", err)
	}
	node := nodes[0]
	pvc := f.MustRWXPVC(ctx, "sec08")
	f.MustPod(ctx, toolsPod("client", pvc.Name, node))
	pv, err := f.PVForClaim(ctx, "sec08")
	if err != nil {
		t.Fatalf("finding the volume behind the claim: %v", err)
	}
	src, err := framework.ExtractNFSSource(pv)
	if err != nil {
		blocked(t, "%v", err)
	}

	var record strings.Builder
	fmt.Fprintf(&record, "export: %s\nnode: %s\n\n", src, node)
	read := 0

	// 1. What the mount itself negotiated. Read back from the node rather than
	// taken from the StorageClass: an option the driver dropped is exactly the
	// kind of difference this record exists to show. Kubelet puts the volume's
	// name in the mount path, not the claim's, which is what is looked up here.
	if m, err := f.PodVolumeMount(ctx, node, pv.Name); err != nil {
		fmt.Fprintf(&record, "mount options: could not be read: %v\n", err)
		t.Logf("reading the mount of the claim on %s: %v", node, err)
	} else {
		read++
		sec, ok := m.OptionValue("sec")
		if !ok {
			sec = "unstated, which the client resolves to sys"
		}
		proto, _ := m.OptionValue("proto")
		xprt, secured := m.OptionValue("xprtsec")
		if !secured {
			xprt = "absent"
		}
		fmt.Fprintf(&record, "mount options: %s\n  sec=%s\n  proto=%s\n  xprtsec=%s\n", m.Options, sec, proto, xprt)
		t.Logf("recorded: the mount on %s carries sec=%s over %s, and xprtsec is %s. Under AUTH_SYS the "+
			"uid and gid on every request are asserted by the client and taken on trust by the server, "+
			"and without transport security both the credentials and the file data cross the network "+
			"in cleartext. That is the ordinary configuration of NFS, and it is what SEC-01 and SEC-02 "+
			"are measuring the behaviour of", node, sec, proto, xprt)
	}

	// 2. Whether the server offers anything better, which is a statement about
	// the deployment rather than about the mount this claim happened to get.
	server := serverPodOrBlock(ctx, t, f)
	if conns, err := framework.ServerConns(ctx, f.C, server); err != nil {
		fmt.Fprintf(&record, "\nlistening ports: could not be read: %v\n", err)
		t.Logf("reading the socket table of %s/%s: %v", server.Namespace, server.Name, err)
	} else {
		read++
		ports := framework.ListeningPorts(conns)
		fmt.Fprintf(&record, "\nlistening ports: %v\n", ports)
		if containsPort(ports, rpcTLSPort) {
			t.Logf("recorded: the server listens on %d as well as %d, so RPC-over-TLS is offered and a "+
				"confidential mount is available to a client that asks for one", rpcTLSPort, nfsPort)
		} else {
			t.Logf("recorded: the server listens on %v and not on %d, so RPC-over-TLS is not offered "+
				"and no client of this deployment can have a confidential data path, whatever it "+
				"configures", ports, rpcTLSPort)
		}
	}

	// 3. Who can reach the port. A pod with no claim, no volume and no
	// relationship to the storage: if it can open a connection, then the only
	// thing between an arbitrary workload and the export is whatever the
	// server's own rules do, which is SEC-05's subject.
	f.MustPod(ctx, framework.PodSpec{Name: "stranger", Node: node})
	host := src.Server
	if h, ok := framework.HostAddr(src); ok {
		host = h.String()
	}
	r := f.Sh(ctx, "stranger", fmt.Sprintf("nc -z -w %d %s %d 2>&1; echo RC=$?",
		int(dataPathProbeTimeout.Seconds()), framework.Quote(host), nfsPort))
	switch out := strings.TrimSpace(r.Combined()); {
	case strings.Contains(out, "RC=0"):
		read++
		fmt.Fprintf(&record, "\nreachability from a pod with no claim: connected to %s:%d\n", host, nfsPort)
		t.Logf("recorded: a pod with no claim and no volume opened a connection to %s:%d. Nothing in "+
			"the network path restricts who may speak to the server, so what refuses an uninvited "+
			"client is the export's own rules and nothing else; SEC-05 asks whether it does",
			host, nfsPort)
	case strings.Contains(strings.ToLower(out), "applet not found"), strings.Contains(strings.ToLower(out), "not found"),
		strings.Contains(strings.ToLower(out), "usage"), strings.Contains(strings.ToLower(out), "invalid option"):
		fmt.Fprintf(&record, "\nreachability from a pod with no claim: not probed, the image's nc could "+
			"not do it: %s\n", out)
		t.Logf("the tools image cannot attempt a connection (%s), so this reading was not taken. It is "+
			"recorded as not taken rather than as unreachable: an instrument nobody verified reports "+
			"silence, not an answer (F-006)", out)
	default:
		read++
		fmt.Fprintf(&record, "\nreachability from a pod with no claim: refused or unreachable: %s\n", out)
		t.Logf("recorded: a pod with no claim could not reach %s:%d (%s), so something in the network "+
			"path is restricting who may speak to the server", host, nfsPort, out)
	}

	// 4. Whether anything in the API says who may. Read from the server's own
	// namespace, which is where a policy protecting it would live.
	if policies, err := f.C.Kube.NetworkingV1().NetworkPolicies(server.Namespace).
		List(ctx, metav1.ListOptions{}); err != nil {
		fmt.Fprintf(&record, "\nnetwork policies in %s: could not be listed: %v\n", server.Namespace, err)
		t.Logf("listing NetworkPolicies in %s: %v", server.Namespace, err)
	} else {
		read++
		names := make([]string, 0, len(policies.Items))
		for _, p := range policies.Items {
			names = append(names, p.Name)
		}
		sort.Strings(names)
		fmt.Fprintf(&record, "\nnetwork policies in %s: %v\n", server.Namespace, names)
		if len(names) == 0 {
			t.Logf("recorded: namespace %s carries no NetworkPolicy, so nothing in the Kubernetes API "+
				"restricts who may open a connection to the server. Whether the CNI would enforce one "+
				"is a separate question the suite answers as a capability", server.Namespace)
		} else {
			t.Logf("recorded: namespace %s carries NetworkPolicies %v. What they permit is not read "+
				"here; that they exist is the fact, and the reachability reading above is what they "+
				"amount to in practice for a pod in %s", server.Namespace, names, framework.Namespace)
		}
	}

	fmt.Fprintf(&record, "\nno packet capture was taken: it needs tcpdump on a node image that has "+
		"none, and an unverified instrument reports silence rather than evidence (F-006). The four "+
		"readings above are what replaces it.\n")
	if err := f.WriteArtifact("data-path.txt", []byte(record.String())); err != nil {
		t.Logf("writing the data path record: %v", err)
	}
	if read == 0 {
		t.Fatalf("none of the four readings could be taken, so this case recorded nothing about the "+
			"data path of %s. A case whose whole output is a record fails when the record is empty, "+
			"because an empty record reads exactly like a deployment nobody found anything wrong with", src)
	}
}

// containsPort reports whether a port is in a list.
func containsPort(ports []uint16, want uint16) bool {
	for _, p := range ports {
		if p == want {
			return true
		}
	}
	return false
}
