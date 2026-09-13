package chaos

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/framework"
)

// The pattern is what a SIGKILL is aimed at, on a node, in the host PID
// namespace. Getting it wrong does not fail a test, it kills the wrong process
// on a real cluster, so the rules are unit tested.

// TestProcessPatternFromContainerCommand covers the ordinary case: the process
// to signal comes from the container's own command, which is the only place the
// cluster states it.
//
// Steps:
//  1. Offer a container whose command is a server binary with arguments.
//  2. Assert the pattern is the base name of that binary.
func TestProcessPatternFromContainerCommand(t *testing.T) {
	got, err := ProcessPattern(Target{Pod: "nfs-server-0", Containers: []corev1.Container{
		{Command: []string{"/usr/bin/ganesha.nfsd", "-F", "-L", "/dev/stdout"}},
	}})
	if err != nil {
		t.Fatalf("deriving the process pattern: %v", err)
	}
	if got != "ganesha.nfsd" {
		t.Errorf("pattern is %q, want the base name of the container command", got)
	}
}

// TestProcessPatternSkipsWrappers covers the case that would do real damage. A
// container started through a shell says nothing about which process ends up
// serving NFS, and killing everything matching "sh" on a node takes the node
// out of service.
//
// Steps:
//  1. Offer a container that starts through a shell.
//  2. Assert it is refused, and that the error says how to fix it.
func TestProcessPatternSkipsWrappers(t *testing.T) {
	// A container that starts through a shell says nothing about which process
	// ends up serving NFS, and killing everything matching "sh" on a node takes
	// the node out.
	_, err := ProcessPattern(Target{Pod: "nfs-server-0", Containers: []corev1.Container{
		{Command: []string{"/bin/sh", "-c", "exec /usr/bin/ganesha.nfsd -F"}},
	}})
	if err == nil {
		t.Fatal("a shell wrapper was accepted as the server process")
	}
	if !strings.Contains(err.Error(), "-server-process") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

// TestProcessPatternNeedsACommand covers the server whose command lives in the
// image entrypoint, where the cluster cannot see it. The case reports blocked
// rather than guessing at a name to kill.
//
// Steps:
//  1. Offer a container with an image and no command.
//  2. Assert no pattern is produced.
func TestProcessPatternNeedsACommand(t *testing.T) {
	// The command is in the image entrypoint, where the cluster cannot see it.
	_, err := ProcessPattern(Target{Pod: "nfs-server-0", Containers: []corev1.Container{{Image: "nfs:1"}}})
	if err == nil {
		t.Fatal("a container with no command yielded a pattern to kill")
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

// TestProcessPatternValidatesTheFlag covers the override. A derived name is
// checked, so a passed one has to be checked too: otherwise the guard that
// stops a chaos case taking a node out of service is one flag away from being
// bypassed, and the flag is the path a hurried operator reaches for.
//
// Steps:
//  1. Pass a generic name through -server-process and assert it is refused.
//  2. Assert the refusal explains the consequence rather than just saying no.
//  3. Pass a real server name and assert it wins over the container command.
func TestProcessPatternValidatesTheFlag(t *testing.T) {
	target := Target{Pod: "nfs-server-0", Containers: []corev1.Container{
		{Command: []string{"/usr/bin/ganesha.nfsd"}},
	}}
	original := framework.Cfg().ServerProcess
	defer func() { framework.Cfg().ServerProcess = original }()

	for _, generic := range []string{"sh", "bash", "env", "run"} {
		framework.Cfg().ServerProcess = generic
		got, err := ProcessPattern(target)
		if err == nil {
			t.Errorf("-server-process=%q was accepted and would be signalled as %q", generic, got)
			continue
		}
		if !strings.Contains(err.Error(), "node") {
			t.Errorf("the refusal of %q does not say what it would cost: %v", generic, err)
		}
	}

	framework.Cfg().ServerProcess = "nfsd"
	got, err := ProcessPattern(target)
	if err != nil {
		t.Fatalf("a real server name was refused: %v", err)
	}
	if got != "nfsd" {
		t.Errorf("pattern is %q, want the flag to win over the container command", got)
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
