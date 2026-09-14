package framework

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SEC-05 asks what happens when a client the export never named attempts a
// mount. Answering it means mounting NFS outside kubelet, which is the one
// genuinely new hazard in the security phase, and three rules contain it.
//
// It never runs in the host mount namespace. F-005 is a node image's mount
// helper doubling the host mount table until the node goes to D-state, and
// F-003 is the same helper making every unmount fail; both live on the path an
// nsenter would take. The probe runs in the node agent's own container, where
// the mount is the kernel's, no helper is involved, and anything left behind
// dies with the container rather than with the node.
//
// It is soft where every other mount in this suite is hard, because a probe
// whose whole purpose is to attempt something that may be refused must not be
// able to retry forever, and because the export it targets may be deleted at
// teardown while it is still mounted (F-001).
//
// And it proves its instrument before it reports a refusal. A mount that failed
// for a client-side reason is indistinguishable from an export refusing a
// stranger: both are EACCES. A probe from a client the server has already
// granted is therefore run first, and a refusal is only reported as one when
// that control succeeded.

const (
	// probeMountOptions are the options every probe mount is made with.
	//
	// soft, and a short timeout with a single retransmission, so the probe
	// fails rather than blocking: the mount is being made from a context the
	// export may refuse, and on a hard mount a refusal at the wrong moment is
	// an uninterruptible retry loop. vers=4.1 because that is the version
	// preflight pinned and the only one any case here is about.
	probeMountOptions = "vers=4.1,soft,timeo=50,retrans=1"

	// probeMountTimeout bounds one attempt, including the read and the
	// unmount. Comfortably above the soft timeout above, so that a probe that
	// came back through the mount options is reported as a refusal rather than
	// as a harness timeout, and well below any case budget.
	probeMountTimeout = 60 * time.Second
)

// MountProbe is the outcome of one attempt to mount an export.
type MountProbe struct {
	// Granted is true when the mount succeeded.
	Granted bool
	// Sum is the checksum of the file the probe was asked to read, present
	// only when the mount was granted and the read succeeded. It is what turns
	// "the mount succeeded" into evidence that another workload's bytes were
	// readable, rather than an inference about an empty directory.
	Sum string
	// Output is everything the probe printed, kept for the report.
	Output string
	// MountMessage is what mount itself said, which is the text a refusal has
	// to be attributed from.
	MountMessage string
	// Unmounted records that the probe left nothing mounted. False after a
	// refusal, because nothing was mounted to leave.
	Unmounted bool
}

// String renders a probe outcome for a log line.
func (p MountProbe) String() string {
	if p.Granted {
		return fmt.Sprintf("granted (checksum %s)", shortSum(p.Sum))
	}
	return fmt.Sprintf("refused: %s", p.MountMessage)
}

// DeniedByServer reports whether a refusal can be attributed to the server
// rather than to the client that made the attempt.
//
// EACCES is what an NFSv4 server's refusal reaches userspace as, and it is also
// what a container that cannot mount produces, so the text alone cannot settle
// it. This is deliberately only half the answer: the case pairs it with a
// control probe from a client the server has already granted, and reports a
// refusal only when that control succeeded. Without the control, a probe that
// could never mount anything would report every export as well defended.
func (p MountProbe) DeniedByServer() bool {
	if p.Granted {
		return false
	}
	low := strings.ToLower(p.MountMessage)
	for _, marker := range []string{"access denied", "permission denied", "not permitted", "no such file"} {
		if strings.Contains(low, marker) {
			return true
		}
	}
	return false
}

// ProbeMount attempts to mount an NFS source from the node agent's container on
// a node, reads a file if the mount is granted, and unmounts.
//
// The caller says which node, because the node is the client identity the
// server sees: the whole point of SEC-05 is the difference between a node the
// export has already served and one it has not.
func (f *Framework) ProbeMount(ctx context.Context, node string, src NFSSource, relPath, id string) (MountProbe, error) {
	agent, err := NodeAgent(ctx, f.C)
	if err != nil {
		return MountProbe{}, Blockedf("the node agent is needed to attempt a mount from outside kubelet: %v", err)
	}
	if err := CheckScriptID(id); err != nil {
		return MountProbe{}, err
	}
	// Run-scoped, because the node agent DaemonSet outlives any one run. Two
	// runs against the same cluster, or a retry after a cancelled probe, would
	// otherwise share a mountpoint and a script path: one invocation would
	// overwrite the other's script, or unmount the other's mount.
	probeID := f.Name(id)
	if err := CheckScriptID(probeID); err != nil {
		return MountProbe{}, err
	}
	if relPath == "" {
		relPath = "-"
	}
	if err := checkProbePath(relPath); err != nil {
		return MountProbe{}, err
	}
	mountPoint := "/tmp/nfsv-probe-" + probeID
	script, err := RunScript("probe-mount.sh", probeID, src.String(), mountPoint, probeMountOptions, relPath)
	if err != nil {
		return MountProbe{}, err
	}
	// Bounded on its own clock. A probe that hangs is a probe whose mount did
	// not honour soft, and waiting it out on the case's budget would spend the
	// case rather than reporting the fact.
	runCtx, cancel := context.WithTimeout(ctx, probeMountTimeout)
	defer cancel()
	out, runErr := agent.RunInContainer(runCtx, node, script)

	probe := parseMountProbe(out)
	if runErr != nil && probe.MountMessage == "" {
		return probe, fmt.Errorf("attempting a mount of %s from %s: %w: %s",
			src, node, runErr, truncate(out, 200))
	}
	if probe.Granted && !probe.Unmounted {
		// A granted probe that could not unmount has left an NFS mount in the
		// agent's container. Say so loudly: it is soft, so it cannot wedge the
		// node, but the next thing that deletes this export would otherwise be
		// operating under a live mount.
		return probe, fmt.Errorf("the probe mounted %s on %s and could not unmount it; the agent's "+
			"container now holds a soft mount of an export the case is about to delete: %s",
			src, node, truncate(out, 200))
	}
	return probe, nil
}

// checkProbePath rejects anything that would read outside the export the probe
// just mounted.
//
// Quoting is not enough here, and that is the whole point: `framework.Quote`
// stops a path being reinterpreted as shell, it does not stop it being a
// different path. This argument is concatenated under the mountpoint and read
// by `sha256sum` in the node agent's **privileged** container, so an absolute
// path or a `..` component turns SEC-05's evidence step into an arbitrary host
// read. AGENTS.md asks for exactly this on any identifier that becomes part of a
// filename: a helper that is safe only because of who calls it today is one
// refactor away from not being safe at all.
//
// "-" is the caller's way of saying "mount only, read nothing", and is allowed.
func checkProbePath(rel string) error {
	if rel == "-" {
		return nil
	}
	if rel == "" {
		return fmt.Errorf("the probe was given an empty path to read; pass \"-\" to skip the read")
	}
	if strings.HasPrefix(rel, "/") {
		return fmt.Errorf("the probe path %q is absolute; it is joined under the probe's own mountpoint, "+
			"so an absolute path reads the agent container's filesystem rather than the export", rel)
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return fmt.Errorf("the probe path %q climbs out of the mount with %q; the probe reads from a "+
				"privileged container, so it may only name a file inside the export it mounted", rel, "..")
		}
	}
	return nil
}

// parseMountProbe reads the token lines scripts/probe-mount.sh prints.
func parseMountProbe(out string) MountProbe {
	probe := MountProbe{Output: out}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "MOUNT_RC":
			rc, err := strconv.Atoi(strings.TrimSpace(value))
			probe.Granted = err == nil && rc == 0
		case "MOUNT_OUT":
			probe.MountMessage = strings.TrimSpace(value)
		case "SHA":
			probe.Sum = strings.TrimSpace(value)
		case "READ_OUT":
			probe.MountMessage = strings.TrimSpace(value)
		case "UMOUNT_RC":
			rc, err := strconv.Atoi(strings.TrimSpace(value))
			probe.Unmounted = err == nil && rc == 0
		}
	}
	if probe.MountMessage == "" && !probe.Granted {
		probe.MountMessage = "the probe printed no mount status at all, so nothing was attempted"
	}
	return probe
}

// shortSum keeps a checksum quotable in a message.
func shortSum(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	if sum == "" {
		return "none"
	}
	return sum
}

// RunInContainer runs a shell snippet inside the node agent's own container on
// a node, rather than in the host namespaces Run enters.
//
// Every other agent call wants the host: /proc/mounts, dmesg and process
// signals are only real there. The mount probe wants the opposite, and the
// difference is not a detail. A mount made in the host mount namespace goes
// through the node image's mount helper, which is where F-005 and F-003 both
// live, and survives the pod that made it. A mount made here is the kernel's,
// is confined to this container, and goes away with it.
func (a *Agent) RunInContainer(ctx context.Context, node, script string) (string, error) {
	pod, ok := a.pods[node]
	if !ok {
		if err := a.refresh(ctx); err != nil {
			return "", err
		}
		if pod, ok = a.pods[node]; !ok {
			return "", fmt.Errorf("no node agent running on %s", node)
		}
	}
	r := a.c.Sh(ctx, Namespace, pod, "agent", script)
	if r.Err != nil {
		return r.Combined(), fmt.Errorf("node %s: %w: %s", node, r.Err, r.Combined())
	}
	return r.Stdout, nil
}
