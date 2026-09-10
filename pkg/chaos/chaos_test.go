package chaos

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
