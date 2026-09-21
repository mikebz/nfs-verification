package chaos

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/env"
	"github.com/mikebz/nfs-verification/pkg/framework"
)

// The pattern is what a SIGKILL is aimed at, on a node, in the host PID
// namespace. Getting it wrong does not fail a test, it kills the wrong process
// on a real cluster, so the rules are unit tested.

// TestChooseProcessUsesWhatPreflightObserved covers the rule and the deployment
// that made it matter. A supervised server declares the supervisor's command,
// or no command at all, while the process actually serving NFS is its child:
// aiming at the declared command kills the wrong process and reports the result
// as an NFS server fault. So the observed name is not merely preferred over the
// declared command, it is the only thing that can name a process to kill. See
// F-026.
//
// Steps:
//  1. Offer a target whose declared command is the supervisor, plus the name
//     preflight observed holding the listening socket.
//  2. Assert the recorded name wins, and that the source says where it came from.
//  3. Drop the recorded name, leaving the declared command in place, and assert
//     that nothing is signalled: the command must not stand in for an
//     observation, however plausible it looks.
func TestChooseProcessUsesWhatPreflightObserved(t *testing.T) {
	target := Target{Pod: "nfs-server-0", Containers: []corev1.Container{
		{Name: "nfs", Command: []string{"/usr/bin/nfs-provisioner", "-provisioner=example.com/nfs"}},
	}}

	got, err := chooseProcess("ganesha.nfsd", "", target)
	if err != nil {
		t.Fatalf("choosing a process to signal: %v", err)
	}
	if got.Pattern != "ganesha.nfsd" {
		t.Errorf("pattern is %q, want the process holding the socket rather than the container command", got.Pattern)
	}
	if !strings.Contains(got.Source, "observed") {
		t.Errorf("source is %q, want it to say the name was observed", got.Source)
	}

	got, err = chooseProcess("", "exec forbidden", target)
	if err == nil {
		t.Fatalf("with nothing observed, the declared command was signalled as %q; killing the supervisor "+
			"stops the pod and every recovery measured afterwards is a container restart (F-026)", got.Pattern)
	}
	if !strings.Contains(err.Error(), "exec forbidden") {
		t.Errorf("the failure does not say why nothing was recorded: %v", err)
	}
}

// TestRecordedProcessMatchesTheTargetPod covers the lookup between a fault's
// target and what preflight wrote down. With fan-out greater than one the
// record holds several servers, and taking the first would name the process in
// a pod the fault is not aimed at, which is a kill aimed at a server that was
// never under test. The reason string matters as much as the name: it is what
// the blocked message carries.
//
// Steps:
//  1. Offer a record of two server pods and ask for the second.
//  2. Assert its own name comes back.
//  3. Ask for a pod the record does not hold, and assert the reason says to
//     re-run preflight rather than implying the server has no process.
func TestRecordedProcessMatchesTheTargetPod(t *testing.T) {
	f := &framework.Framework{Env: &env.Environment{Servers: []env.ServerInfo{
		{Namespace: "nfs", Pod: "server-0", Process: "unfsd"},
		{Namespace: "nfs", Pod: "server-1", Process: "ganesha.nfsd"},
		{Namespace: "nfs", Pod: "server-2", ProcessNote: "pod is Pending"},
	}}}

	if got, why := recordedProcess(f, Target{Namespace: "nfs", Pod: "server-1"}); got != "ganesha.nfsd" {
		t.Errorf("named %q (%s), want the process recorded for that pod and not for another", got, why)
	}
	if got, why := recordedProcess(f, Target{Namespace: "nfs", Pod: "server-2"}); got != "" || why != "pod is Pending" {
		t.Errorf("named %q because %q, want the recorded reason for having no process", got, why)
	}
	got, why := recordedProcess(f, Target{Namespace: "nfs", Pod: "server-9"})
	if got != "" {
		t.Errorf("named %q for a pod preflight never saw", got)
	}
	if !strings.Contains(why, "refresh-preflight") {
		t.Errorf("the reason does not say how to fix a record that predates this pod: %q", why)
	}
}

// TestChooseProcessRefusesAGenericName covers the case that would do real
// damage. Preflight names whatever holds the socket, and on an image that
// serves through a wrapper that name could be "sh"; the pattern is matched
// node-wide in the host PID namespace, so signalling it kills every shell on
// the node and takes it out of service. Observed is not the same as safe, and
// the guard applies to a recorded name exactly as it would to any other.
//
// Steps:
//  1. Offer a generic recorded name.
//  2. Assert it is refused rather than signalled because preflight wrote it down.
//  3. Assert the refusal names what was recorded and says what it would cost.
func TestChooseProcessRefusesAGenericName(t *testing.T) {
	target := Target{Pod: "nfs-server-0", Containers: []corev1.Container{
		{Name: "nfs", Command: []string{"/bin/sh", "-c", "exec /usr/bin/ganesha.nfsd -F"}},
	}}
	for _, generic := range []string{"sh", "bash", "env", "run", "tini"} {
		got, err := chooseProcess(generic, "", target)
		if err == nil {
			t.Errorf("%q was accepted and would be signalled as %q; killing every process of that name on "+
				"a node takes the node out", generic, got.Pattern)
			continue
		}
		if !strings.Contains(err.Error(), "node") {
			t.Errorf("the refusal of %q does not say what it would cost: %v", generic, err)
		}
	}
}

// TestChooseProcessNeedsPreflight covers the server preflight could not read:
// no listener it could attribute, or a record written before this pod existed.
// The case reports blocked rather than guessing at a name to kill, and the
// message has to send the reader to preflight, since that is now the only place
// the answer can come from.
//
// Steps:
//  1. Ask for a pattern with nothing recorded and a reason for it.
//  2. Assert no pattern is produced.
//  3. Assert the failure carries that reason and says to re-run preflight.
func TestChooseProcessNeedsPreflight(t *testing.T) {
	_, err := chooseProcess("", "nothing is listening on port 2049 here",
		Target{Pod: "nfs-server-0", Containers: []corev1.Container{{Name: "nfs", Image: "nfs:1"}}})
	if err == nil {
		t.Fatal("a pattern to kill was produced with nothing recorded")
	}
	if !strings.Contains(err.Error(), "nothing is listening") {
		t.Errorf("the failure does not say why nothing was recorded: %v", err)
	}
	if !strings.Contains(err.Error(), "refresh-preflight") {
		t.Errorf("the failure does not say how to get an answer: %v", err)
	}
}

// TestUsableAsPattern covers the accept and reject lists directly, since this
// is the rule standing between a chaos case and killing the wrong process on
// somebody's cluster.
//
// Steps:
//  1. Assert the names real NFS servers run under are accepted.
//  2. Assert shells, wrappers, empty and short names are refused.
func TestUsableAsPattern(t *testing.T) {
	for _, name := range []string{"ganesha.nfsd", "nfsd", "unfsd", "rpc.nfsd"} {
		if !usableAsPattern(name) {
			t.Errorf("%q was rejected, but it names a server process", name)
		}
	}
	for _, name := range []string{"", "sh", "bash", "env", "tini", "run", "start.sh", "entrypoint.sh"} {
		if usableAsPattern(name) {
			t.Errorf("%q was accepted; killing everything matching it on a node takes the node out", name)
		}
	}
}

// TestFirstInjurableSkipsUnfitPods covers which pod a fault gets aimed at
// when the candidates sorted ahead of the healthy one cannot be injured.
//
// Discovery sorts by name and deliberately keeps both terminating and Pending
// pods, because a pod on its way out still belongs in the artifact bundle and a
// server that cannot start is worth reporting. Selection must not inherit that.
// With fan-out greater than one, the pods sorted first can be one a previous
// cycle deleted and one that has not come up yet, while a healthy pod sits
// behind them. Aiming at the first measures something this case did not cause
// and attributes it to a fault it believes it caused.
//
// Not a failure any run has produced, because the deployment under test runs a
// single-replica StatefulSet whose replacement reuses the name, so the two
// never coexist. It is the same confusion as F-013 one level up, and the guard
// is small, so it is cheaper to hold down than to rediscover.
//
// Steps:
//  1. Select from nothing, and from a single healthy pod.
//  2. Select where a terminating pod sorts ahead of a healthy one.
//  3. Select where a Pending pod sorts ahead of a healthy one.
//  4. Select where both sort ahead of the healthy one.
//  5. Select where no candidate is fit, for mixed reasons.
//  6. Assert the healthy pod is chosen whenever one exists, that a not-ready
//     Running pod still counts as one, and that the two empty-handed answers
//     are distinguishable from each other.
func TestFirstInjurableSkipsUnfitPods(t *testing.T) {
	now := metav1.Now()
	pod := func(name string, phase corev1.PodPhase, terminating bool) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
		p.Status.Phase = phase
		if terminating {
			p.DeletionTimestamp = &now
		}
		return p
	}
	healthy := func(name string) corev1.Pod { return pod(name, corev1.PodRunning, false) }
	dying := func(name string) corev1.Pod { return pod(name, corev1.PodRunning, true) }
	starting := func(name string) corev1.Pod { return pod(name, corev1.PodPending, false) }
	dead := func(name string) corev1.Pod { return pod(name, corev1.PodFailed, false) }

	if _, err := firstInjurable(nil); err == nil {
		t.Error("selecting from no pods returned a target")
	} else if !strings.Contains(err.Error(), "-server-selector") {
		t.Errorf("the empty-discovery error does not name the flags that fix it: %v", err)
	}

	// Each of these must reach nfs-server-9: the pods ahead of it are the ones
	// discovery keeps and selection must refuse.
	picks := []struct {
		name string
		pods []corev1.Pod
	}{
		{"the only pod, healthy", []corev1.Pod{healthy("nfs-server-9")}},
		{"past a terminating pod", []corev1.Pod{dying("nfs-server-0"), healthy("nfs-server-9")}},
		{"past a pending pod", []corev1.Pod{starting("nfs-server-0"), healthy("nfs-server-9")}},
		// The rule is "not Running", not "not Pending". Discovery cannot
		// currently hand this one over, and the guard does not depend on that
		// staying true.
		{"past a failed pod", []corev1.Pod{dead("nfs-server-0"), healthy("nfs-server-9")}},
		{"past both", []corev1.Pod{dying("nfs-server-0"), starting("nfs-server-1"), healthy("nfs-server-9")}},
	}
	for _, tc := range picks {
		t.Run(tc.name, func(t *testing.T) {
			got, err := firstInjurable(tc.pods)
			if err != nil {
				t.Fatalf("selecting %s: %v", tc.name, err)
			}
			if got.Name != "nfs-server-9" {
				t.Errorf("selected %q, want nfs-server-9: the pods sorted ahead of it are being deleted "+
					"or have not started, and a fault aimed at one would measure something this case "+
					"did not cause", got.Name)
			}
		})
	}

	// Running but not ready is still a legitimate target, and refusing it would
	// report a sick server as a harness error. Asserted so that tightening the
	// rule to readiness has to be a deliberate change with a reason.
	t.Run("a running pod that is not ready", func(t *testing.T) {
		p := healthy("nfs-server-0")
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		got, err := firstInjurable([]corev1.Pod{p})
		if err != nil {
			t.Fatalf("selecting a running pod that is not ready: %v", err)
		}
		if got.Name != "nfs-server-0" {
			t.Errorf("selected %q, want the not-ready pod", got.Name)
		}
	})

	// Nothing fit is a different fact from nothing found, and has to read
	// differently: this one is not fixed by passing a selector. Each pod is
	// named with its own reason, because "deleted" and "never started" send an
	// operator to different places.
	t.Run("no candidate is fit", func(t *testing.T) {
		_, err := firstInjurable([]corev1.Pod{dying("nfs-server-0"), starting("nfs-server-1")})
		if err == nil {
			t.Fatal("selecting from only unfit pods returned a target")
		}
		for _, want := range []string{"nfs-server-0 is being deleted", "nfs-server-1 is Pending"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not say %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "-server-selector") {
			t.Errorf("the error suggests a discovery flag, but discovery worked and every pod it found "+
				"is unfit: %v", err)
		}
	})
}
