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

// TestFirstInjurableSkipsTerminating covers which pod a fault gets aimed at
// when one of the candidates is already going away.
//
// Discovery sorts by name and deliberately keeps terminating pods, because a
// pod on its way out still belongs in the artifact bundle and its log is still
// worth reading. Selection must not inherit that. With fan-out greater than
// one, the pod sorted first can be one that a previous cycle deleted, while a
// healthy pod sits behind it: aiming at the first measures the tail of that
// deletion and attributes it to a fault this case believes it caused.
//
// Not a failure any run has produced, because the deployment under test runs a
// single-replica StatefulSet whose replacement reuses the name, so the two
// never coexist. It is the same confusion as F-013 one level up, and the guard
// is two lines, so it is cheaper to hold down than to rediscover.
//
// Steps:
//  1. Select from nothing, and from a single healthy pod.
//  2. Select where a terminating pod sorts ahead of a healthy one.
//  3. Select where every candidate is terminating.
//  4. Assert the healthy pod is chosen whenever one exists, and that the two
//     empty-handed answers are distinguishable from each other.
func TestFirstInjurableSkipsTerminating(t *testing.T) {
	now := metav1.Now()
	pod := func(name string, terminating bool) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if terminating {
			p.DeletionTimestamp = &now
		}
		return p
	}

	if _, err := firstInjurable(nil); err == nil {
		t.Error("selecting from no pods returned a target")
	} else if !strings.Contains(err.Error(), "-server-selector") {
		t.Errorf("the empty-discovery error does not name the flags that fix it: %v", err)
	}

	got, err := firstInjurable([]corev1.Pod{pod("nfs-server-0", false)})
	if err != nil {
		t.Fatalf("selecting the only healthy pod: %v", err)
	}
	if got.Name != "nfs-server-0" {
		t.Errorf("selected %q, want the healthy pod", got.Name)
	}

	// Sorted first and terminating, which is the regression: taking pods[0]
	// here aims the fault at a pod that is already leaving.
	got, err = firstInjurable([]corev1.Pod{pod("nfs-server-0", true), pod("nfs-server-1", false)})
	if err != nil {
		t.Fatalf("selecting past a terminating pod: %v", err)
	}
	if got.Name != "nfs-server-1" {
		t.Errorf("selected %q, want nfs-server-1: the pod sorted first is already being deleted, and a "+
			"fault aimed at it would measure that deletion rather than one this case caused", got.Name)
	}

	// All terminating is a different fact from none found, and has to read
	// differently: this one is not fixed by passing a selector.
	_, err = firstInjurable([]corev1.Pod{pod("nfs-server-0", true), pod("nfs-server-1", true)})
	if err == nil {
		t.Fatal("selecting from only terminating pods returned a target")
	}
	if !strings.Contains(err.Error(), "nfs-server-0") || !strings.Contains(err.Error(), "nfs-server-1") {
		t.Errorf("the error does not name the pods it rejected: %v", err)
	}
	if strings.Contains(err.Error(), "-server-selector") {
		t.Errorf("the error suggests a discovery flag, but discovery worked and every pod it found is "+
			"being deleted: %v", err)
	}
}
