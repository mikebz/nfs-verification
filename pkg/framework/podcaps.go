package framework

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// What a pod spec declares and what its process holds are two different facts,
// and SEC-09 is about the gap between them. An admission policy, a runtime
// default or a bounding-set restriction can each remove a capability the
// workload asked for, and the result is not a pod that fails to start: it is a
// server that runs and then fails the operations needing that capability, with
// EPERM, at the point a client asks for them.
//
// So the declared set is read from the API and the effective set from the
// process, and the case compares them.

// capNames maps a capability bit to its name, from capabilities(7). The list
// stops where the kernel's does; a bit beyond it is rendered numerically rather
// than dropped, because an unknown capability in an effective set is exactly
// the thing a reader wants to see.
var capNames = []string{
	"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID",
	"KILL", "SETGID", "SETUID", "SETPCAP", "LINUX_IMMUTABLE",
	"NET_BIND_SERVICE", "NET_BROADCAST", "NET_ADMIN", "NET_RAW", "IPC_LOCK",
	"IPC_OWNER", "SYS_MODULE", "SYS_RAWIO", "SYS_CHROOT", "SYS_PTRACE",
	"SYS_PACCT", "SYS_ADMIN", "SYS_BOOT", "SYS_NICE", "SYS_RESOURCE",
	"SYS_TIME", "SYS_TTY_CONFIG", "MKNOD", "LEASE", "AUDIT_WRITE",
	"AUDIT_CONTROL", "SETFCAP", "MAC_OVERRIDE", "MAC_ADMIN", "SYSLOG",
	"WAKE_ALARM", "BLOCK_SUSPEND", "AUDIT_READ", "PERFMON", "BPF",
	"CHECKPOINT_RESTORE",
}

// CapSet is the capability state of a running process, as /proc/<pid>/status
// reports it.
type CapSet struct {
	Inheritable uint64
	Permitted   uint64
	Effective   uint64
	Bounding    uint64
	Ambient     uint64
}

// CapNames renders a mask as sorted capability names.
func CapNames(mask uint64) []string {
	var out []string
	for bit := 0; bit < 64; bit++ {
		if mask&(1<<uint(bit)) == 0 {
			continue
		}
		if bit < len(capNames) {
			out = append(out, capNames[bit])
			continue
		}
		out = append(out, fmt.Sprintf("CAP_%d", bit))
	}
	sort.Strings(out)
	return out
}

// HasCap reports whether a mask carries a named capability, with or without the
// CAP_ prefix the Kubernetes API drops and the kernel documentation keeps.
func HasCap(mask uint64, name string) bool {
	want := strings.ToUpper(strings.TrimPrefix(strings.ToUpper(name), "CAP_"))
	for bit, n := range capNames {
		if n == want {
			return mask&(1<<uint(bit)) != 0
		}
	}
	return false
}

// ParseProcStatus reads the capability masks out of /proc/<pid>/status.
//
// A status file with no Cap lines at all is an error rather than an empty set:
// the two are indistinguishable in a struct of zeroes, and reporting "the
// server holds no capabilities" because the read went wrong would file a
// harness failure as a deployment finding.
func ParseProcStatus(contents string) (CapSet, error) {
	var caps CapSet
	found := 0
	for _, line := range strings.Split(contents, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		var target *uint64
		switch strings.TrimSpace(key) {
		case "CapInh":
			target = &caps.Inheritable
		case "CapPrm":
			target = &caps.Permitted
		case "CapEff":
			target = &caps.Effective
		case "CapBnd":
			target = &caps.Bounding
		case "CapAmb":
			target = &caps.Ambient
		default:
			continue
		}
		mask, err := strconv.ParseUint(value, 16, 64)
		if err != nil {
			return CapSet{}, fmt.Errorf("capability mask %q: %w", value, err)
		}
		*target = mask
		found++
	}
	if found == 0 {
		return CapSet{}, fmt.Errorf("no capability lines in %d bytes of process status; "+
			"an empty set and an unread file are the same struct, so this is an error rather than a reading",
			len(contents))
	}
	return caps, nil
}

// ContainerCaps reads the capability set of a container's PID 1.
//
// PID 1 rather than the exec'd shell: the shell inherits the container's set,
// which is usually the same, but the process under test is the server and a
// difference between the two is worth seeing rather than assuming away.
func ContainerCaps(ctx context.Context, c *Client, pod *corev1.Pod) (CapSet, error) {
	container := ""
	if len(pod.Spec.Containers) > 0 {
		container = pod.Spec.Containers[0].Name
	}
	r := c.Sh(ctx, pod.Namespace, pod.Name, container, "cat /proc/1/status")
	if r.Err != nil {
		return CapSet{}, Blockedf("reading the capability set of %s/%s: %v: %s. "+
			"This needs exec into the server's namespace and a container with cat",
			pod.Namespace, pod.Name, r.Err, truncate(r.Combined(), 200))
	}
	caps, err := ParseProcStatus(r.Stdout)
	if err != nil {
		return CapSet{}, fmt.Errorf("reading the capability set of %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return caps, nil
}

// DeclaredCaps is what a container's spec asks for: the capabilities it adds,
// the ones it drops, and whether it asked to be privileged and skip the
// question entirely.
type DeclaredCaps struct {
	Container  string
	Add        []string
	Drop       []string
	Privileged bool
}

// DeclaredCapsOf reads the declaration from a pod's first container, which is
// the one every other server reader in this suite uses.
func DeclaredCapsOf(pod *corev1.Pod) DeclaredCaps {
	if len(pod.Spec.Containers) == 0 {
		return DeclaredCaps{}
	}
	ct := pod.Spec.Containers[0]
	d := DeclaredCaps{Container: ct.Name}
	sc := ct.SecurityContext
	if sc == nil {
		return d
	}
	if sc.Privileged != nil {
		d.Privileged = *sc.Privileged
	}
	if sc.Capabilities == nil {
		return d
	}
	for _, a := range sc.Capabilities.Add {
		d.Add = append(d.Add, string(a))
	}
	for _, dr := range sc.Capabilities.Drop {
		d.Drop = append(d.Drop, string(dr))
	}
	return d
}
