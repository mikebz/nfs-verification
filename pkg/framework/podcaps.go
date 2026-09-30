package framework

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/mikebz/nfs-verification/pkg/env"
)

// What a pod spec declares and what its process holds are two different facts,
// and SEC-09 is about the gap between them. An admission policy, a runtime
// default or a bounding-set restriction can each remove a capability the
// workload asked for, and the result is not a pod that fails to start: it is a
// server that runs and then fails the operations needing that capability, with
// EPERM, at the point a client asks for them.
//
// So the declared set is read from the API and the runtime sets from two
// processes, because they answer two questions. The container's PID 1 is what
// the runtime started, so its set is what the platform delivered. The process
// holding the NFS socket is the server, and its set is what the server actually
// holds, which on a supervised server is not PID 1's (F-026, #98).

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

// CapGaps sorts the declared capabilities the server does not hold by where
// each one went missing, because the two places belong to different people.
//
// The server's own set is asked first. A capability the server holds is not
// missing, whatever PID 1 holds: a supervisor may start the server and then
// drop the capability from its own set, so PID 1 lacking it later proves
// nothing about what the platform delivered, and the server holding it proves
// the platform did.
//
// Of what the server does not hold, stripped is what the container's first
// process does not hold either, so the platform took it between the spec and
// the runtime: an admission policy, a restricted bounding set, or a runtime
// default such as a non-root user with no ambient set. That is SEC-09's
// failure. The one thing the witness cannot rule out is a first process that
// dropped the capability itself before the case read it, and the case's
// failure message says so. Dropped is what the platform delivered and the
// server process no longer holds, so something inside the container gave it
// up, the server or its supervisor. That is the server's own choice, and on
// gke-w2 it is ganesha.nfsd lowering CAP_SYS_RESOURCE at start (#98).
//
// Both are judged on the permitted set rather than the effective one. A process
// may lower a capability from its effective set and raise it again around the
// call that needs it; only one gone from the permitted set is gone for good
// (capabilities(7)).
func CapGaps(declared []string, delivered, held CapSet) (stripped, dropped []string) {
	for _, want := range declared {
		switch {
		case HasCap(held.Permitted, want):
		case !HasCap(delivered.Permitted, want):
			stripped = append(stripped, want)
		default:
			dropped = append(dropped, want)
		}
	}
	return stripped, dropped
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

// ContainerCaps reads the capability set of a container's PID 1, which is the
// process the runtime started and so the witness for what the platform
// delivered to the container.
//
// It is not the server. On a supervised server PID 1 is the supervisor and the
// process serving NFS is its child, with a set of its own: on gke-w2,
// ganesha.nfsd drops CAP_SYS_RESOURCE at start and nfs-provisioner, PID 1,
// keeps it (F-026, #98). ServerProcessCaps reads the server.
//
// A pod whose PID 1 is not its container's first process is refused as blocked
// rather than read; pid1IsNotTheContainers says which pods those are.
func ContainerCaps(ctx context.Context, c *Client, pod *corev1.Pod, container string) (ProcessCaps, error) {
	if err := pid1IsNotTheContainers(pod, container); err != nil {
		return ProcessCaps{}, err
	}
	r := c.Sh(ctx, pod.Namespace, pod.Name, container, "cat /proc/1/status")
	if r.Err != nil {
		return ProcessCaps{}, Blockedf("reading the capability set of PID 1 in %s/%s container %s: %v: %s. "+
			"This needs exec into the server's container and a cat in its image",
			pod.Namespace, pod.Name, container, r.Err, truncate(r.Combined(), 200))
	}
	caps, err := ParseProcStatus(r.Stdout)
	if err != nil {
		return ProcessCaps{}, fmt.Errorf("reading the capability set of PID 1 in %s/%s container %s: %w",
			pod.Namespace, pod.Name, container, err)
	}
	return ProcessCaps{PID: 1, Node: pod.Spec.NodeName, Name: statusName(strings.Split(r.Stdout, "\n")),
		Container: container, Caps: caps}, nil
}

// pid1IsNotTheContainers refuses the pods in which /proc/1, read from inside a
// container, is not the process the runtime started for that container. Each
// of these reads a real set belonging to somebody else, so the failure has no
// symptom: SEC-09 would pass or fail on it without a word.
//
//   - hostPID: PID 1 is the node's init, which on an ordinary node holds every
//     capability, so a stripped one would read as delivered.
//   - A process namespace shared between the pod's containers: PID 1 is the
//     pause process, whose set is the sandbox's rather than this container's,
//     so a declared capability could read as stripped that was delivered.
func pid1IsNotTheContainers(pod *corev1.Pod, container string) error {
	switch {
	case pod.Spec.HostPID:
		return Blockedf("%s/%s runs in the node's process namespace, so PID 1 in container %s is the node's "+
			"init rather than the process the runtime started for it, and what the platform delivered to "+
			"that container cannot be read there", pod.Namespace, pod.Name, container)
	case pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace:
		return Blockedf("%s/%s shares one process namespace between its containers, so PID 1 in container "+
			"%s is the pod's sandbox rather than the process the runtime started for it, and what the "+
			"platform delivered to that container cannot be read there", pod.Namespace, pod.Name, container)
	}
	return nil
}

// procStatusTimeout bounds the node read of one process. One small file pair
// on one node; a node that cannot answer that in this long is not answering.
const procStatusTimeout = 20 * time.Second

// ProcessCaps is the capability set of one process, with what identifies it:
// the name the kernel reports and the container it runs in.
type ProcessCaps struct {
	// PID is the process id: the node's for the server, read through the node
	// agent, and 1 for a container's first process, read from inside it.
	PID int
	// Node is where the process runs.
	Node string
	// Name is the Name line of its status file, which is its comm.
	Name string
	// Container is the pod container the process is in.
	Container string
	Caps      CapSet
}

// ServerProcessCaps reads the capability set of the process serving NFS, from
// the node, and names the container it runs in.
//
// From the node because the pid DiscoverServerProcess returns is the node's,
// and naming the server already needed the node agent (F-027). The read is
// checked against the process it was meant to be: the name has to be the one
// discovery saw and the cgroup has to be one of this pod's containers, so a
// server that restarted between the two reads, leaving its old pid to
// something else, is an error rather than somebody else's capabilities.
//
// A server that preforks holds its socket from several processes of one name,
// and every one of them is read. What the server holds is what all of them
// hold, so the sets are intersected: the lowest pid may be a master that kept
// a capability the workers serving requests have dropped, and reading only it
// would report the server holding what it does not. Processes in different
// containers are an error, since they cannot be one server's.
func ServerProcessCaps(ctx context.Context, agent *Agent, pod *corev1.Pod, sp ServerProcess) (ProcessCaps, error) {
	if agent == nil {
		return ProcessCaps{}, Blockedf("no node agent, so the capability set of %s in %s/%s cannot be read",
			sp.Name, pod.Namespace, pod.Name)
	}
	pids := sp.PIDs
	if len(pids) == 0 {
		pids = []int{sp.PID}
	}
	if pids[0] <= 0 || sp.Node == "" {
		return ProcessCaps{}, fmt.Errorf("the server process in %s/%s has no node pid to read (%s)",
			pod.Namespace, pod.Name, sp)
	}
	reads := make([]ProcessCaps, 0, len(pids))
	for _, pid := range pids {
		r, err := readServerPID(ctx, agent, pod, sp, pid)
		if err != nil {
			return ProcessCaps{}, err
		}
		reads = append(reads, r)
	}
	return intersectServerReads(reads)
}

// readServerPID reads one of the server's processes, with the identity checks
// ServerProcessCaps describes.
func readServerPID(ctx context.Context, agent *Agent, pod *corev1.Pod, sp ServerProcess, pid int) (ProcessCaps, error) {
	readCtx, cancel := context.WithTimeout(ctx, procStatusTimeout)
	defer cancel()
	out, err := agent.RunScript(readCtx, sp.Node, "proc-status.sh",
		"caps-"+strings.ToLower(Cfg().RunID), "/proc", strconv.Itoa(pid))
	if err != nil {
		return ProcessCaps{}, fmt.Errorf("reading pid %d on %s: %w", pid, sp.Node, err)
	}
	read, err := parseProcRead(out)
	if err != nil {
		return ProcessCaps{}, fmt.Errorf("reading pid %d on %s: %w", pid, sp.Node, err)
	}
	if err := sameProcessName(read.name, sp.Name); err != nil {
		return ProcessCaps{}, fmt.Errorf("pid %d on %s: %w", pid, sp.Node, err)
	}
	container, err := containerOf(read.cgroup, pod.Status.ContainerStatuses)
	if err != nil {
		return ProcessCaps{}, fmt.Errorf("pid %d on %s is %s but not in %s/%s: %w",
			pid, sp.Node, read.name, pod.Namespace, pod.Name, err)
	}
	return ProcessCaps{PID: pid, Node: sp.Node, Name: read.name, Container: container, Caps: read.caps}, nil
}

// intersectServerReads combines the reads of every process serving one socket
// into the set the server holds: a capability counts only if every one of
// them holds it. The result carries the lowest pid, as discovery does.
func intersectServerReads(reads []ProcessCaps) (ProcessCaps, error) {
	if len(reads) == 0 {
		return ProcessCaps{}, fmt.Errorf("no server process was read")
	}
	out := reads[0]
	for _, r := range reads[1:] {
		if r.Container != out.Container {
			return ProcessCaps{}, fmt.Errorf("the processes holding the socket run in containers %s (pid %d) and "+
				"%s (pid %d), so they are not one server", out.Container, out.PID, r.Container, r.PID)
		}
		out.Caps.Inheritable &= r.Caps.Inheritable
		out.Caps.Permitted &= r.Caps.Permitted
		out.Caps.Effective &= r.Caps.Effective
		out.Caps.Bounding &= r.Caps.Bounding
		out.Caps.Ambient &= r.Caps.Ambient
		if r.PID < out.PID {
			out.PID = r.PID
		}
	}
	return out, nil
}

// procRead is what scripts/proc-status.sh printed, parsed.
type procRead struct {
	name   string
	cgroup string
	caps   CapSet
}

// parseProcRead reads the script's output. Anything short of both files and
// the end marker is an error: a truncated exec that reads as a process holding
// nothing is F-011's shape, and here it would report a capability as stripped.
func parseProcRead(out string) (procRead, error) {
	var status, cgroup []string
	var section string
	var sawStatus, sawCgroup, complete bool
	var errs []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case section == "status" && line == "==ENDSTATUS":
			section, sawStatus = "", true
		case section == "cgroup" && line == "==ENDCGROUP":
			section, sawCgroup = "", true
		case section == "status":
			status = append(status, line)
		case section == "cgroup":
			cgroup = append(cgroup, line)
		case line == "==STATUS":
			section = "status"
		case line == "==CGROUP":
			section = "cgroup"
		case strings.HasPrefix(line, "==ERROR "):
			errs = append(errs, strings.TrimSpace(strings.TrimPrefix(line, "==ERROR ")))
		case line == "==END":
			complete = true
		}
	}
	if len(errs) > 0 {
		return procRead{}, fmt.Errorf("could not be read: %s", strings.Join(errs, "; "))
	}
	if !complete || !sawStatus || !sawCgroup {
		return procRead{}, fmt.Errorf("the reader did not run to completion (status %v, cgroup %v, end %v), "+
			"so an unread file would read as a process holding nothing", sawStatus, sawCgroup, complete)
	}
	caps, err := ParseProcStatus(strings.Join(status, "\n"))
	if err != nil {
		return procRead{}, err
	}
	name := statusName(status)
	if name == "" {
		return procRead{}, fmt.Errorf("the status file carries no Name line, so which process it describes cannot be checked")
	}
	return procRead{name: name, cgroup: strings.Join(cgroup, "\n"), caps: caps}, nil
}

// statusName returns the Name line of a status file, or nothing if it has none.
func statusName(lines []string) string {
	for _, line := range lines {
		if v, ok := strings.CutPrefix(line, "Name:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// commLen is the longest name /proc/<pid>/status reports: the kernel's comm
// buffer is 16 bytes including its terminator (proc(5)).
const commLen = 15

// sameProcessName checks that the status file describes the process discovery
// named. Discovery prefers argv[0]'s base name where comm was truncated, so a
// comm of the full 15 characters matches any name it begins; anything shorter
// has to match exactly.
func sameProcessName(status, want string) error {
	if status == want || (len(status) == commLen && strings.HasPrefix(want, status)) {
		return nil
	}
	return fmt.Errorf("is now %q, not the %q discovery named, so the server restarted between the two reads "+
		"and its pid belongs to something else", status, want)
}

// containerOf names the pod container whose cgroup a process is in, by finding
// the container's runtime id in the process's cgroup path. Every runtime this
// suite has met puts the full id in the path, whether the kubelet uses the
// systemd or the cgroupfs driver, and matching the id rather than parsing the
// path is what keeps this independent of which.
//
// A container with no id yet is skipped rather than matched: an empty id is a
// substring of every path.
func containerOf(cgroup string, statuses []corev1.ContainerStatus) (string, error) {
	var matched, seen []string
	for _, s := range statuses {
		_, id, ok := strings.Cut(s.ContainerID, "://")
		if !ok || id == "" {
			continue
		}
		seen = append(seen, s.Name+"="+id)
		if strings.Contains(cgroup, id) {
			matched = append(matched, s.Name)
		}
	}
	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return "", fmt.Errorf("its cgroup %q names none of the pod's containers (%s)",
			strings.TrimSpace(cgroup), strings.Join(seen, ", "))
	default:
		return "", fmt.Errorf("its cgroup %q names more than one of the pod's containers (%s)",
			strings.TrimSpace(cgroup), strings.Join(matched, ", "))
	}
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

// DeclaredCapsOf reads the declaration of the named container, which for
// SEC-09 is the one the server process was found running in.
//
// A container the spec does not have is an error rather than an empty
// declaration: SEC-09 checks each declared capability, so a declaration read as
// empty is a case that passes having checked nothing.
func DeclaredCapsOf(pod *corev1.Pod, container string) (DeclaredCaps, error) {
	d := DeclaredCaps{Container: container}
	var ct *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == container {
			ct = &pod.Spec.Containers[i]
			break
		}
	}
	if ct == nil {
		return d, fmt.Errorf("%s/%s has no container %q in its spec", pod.Namespace, pod.Name, container)
	}
	sc := ct.SecurityContext
	if sc == nil {
		return d, nil
	}
	if sc.Privileged != nil {
		d.Privileged = *sc.Privileged
	}
	if sc.Capabilities == nil {
		return d, nil
	}
	for _, a := range sc.Capabilities.Add {
		d.Add = append(d.Add, string(a))
	}
	for _, dr := range sc.Capabilities.Drop {
		d.Drop = append(d.Drop, string(dr))
	}
	return d, nil
}

// ReadServerCapabilities reads everything SEC-09 judges about one server pod,
// at one moment, for preflight to record: the declaration of the container the
// server runs in, the set the server process holds, and the set the container's
// PID 1 holds.
//
// It runs in preflight rather than in the case because the server's set is read
// through the node agent (F-027), and a case should not need elevated
// privilege to evaluate a fact about the platform that it does not change. The
// three are read together so the record never pairs one moment's declaration
// with another moment's process.
func ReadServerCapabilities(ctx context.Context, c *Client, agent *Agent, pod *corev1.Pod, sp ServerProcess) (env.ServerCapabilities, error) {
	server, err := ServerProcessCaps(ctx, agent, pod, sp)
	if err != nil {
		return env.ServerCapabilities{}, fmt.Errorf("reading the capability set of %s: %w", sp.Name, err)
	}
	declared, err := DeclaredCapsOf(pod, server.Container)
	if err != nil {
		return env.ServerCapabilities{}, err
	}
	init, err := ContainerCaps(ctx, c, pod, server.Container)
	if err != nil {
		return env.ServerCapabilities{}, err
	}
	return env.ServerCapabilities{
		PodUID:       string(pod.UID),
		Container:    server.Container,
		DeclaredAdd:  declared.Add,
		DeclaredDrop: declared.Drop,
		Privileged:   declared.Privileged,
		Server:       capMasksOf(server),
		Init:         capMasksOf(init),
	}, nil
}

// capMasksOf renders a process's set in the form /proc/<pid>/status prints.
func capMasksOf(p ProcessCaps) env.CapMasks {
	hex := func(m uint64) string { return fmt.Sprintf("%016x", m) }
	return env.CapMasks{
		Name:        p.Name,
		Inheritable: hex(p.Caps.Inheritable),
		Permitted:   hex(p.Caps.Permitted),
		Effective:   hex(p.Caps.Effective),
		Bounding:    hex(p.Caps.Bounding),
		Ambient:     hex(p.Caps.Ambient),
	}
}

// CapSetOf decodes a recorded set. A mask that does not parse is an error, not
// an empty set, for ParseProcStatus's reason: an empty set would read as every
// declared capability stripped.
func CapSetOf(m env.CapMasks) (CapSet, error) {
	var caps CapSet
	for _, f := range []struct {
		name  string
		value string
		into  *uint64
	}{
		{"inheritable", m.Inheritable, &caps.Inheritable},
		{"permitted", m.Permitted, &caps.Permitted},
		{"effective", m.Effective, &caps.Effective},
		{"bounding", m.Bounding, &caps.Bounding},
		{"ambient", m.Ambient, &caps.Ambient},
	} {
		v, err := strconv.ParseUint(f.value, 16, 64)
		if err != nil {
			return CapSet{}, fmt.Errorf("recorded %s mask of %s %q: %w", f.name, m.Name, f.value, err)
		}
		*f.into = v
	}
	return caps, nil
}

// StaleCapabilityRecord says why a recorded capability read no longer describes
// the live pod, or returns nothing when it still does.
//
// A preflight record is reused for up to -preflight-max-age, and a StatefulSet
// recreates a pod under the same name. The recorded sets follow from the pod
// spec and the node's runtime, and the security context, the process
// namespace and the node cannot change without a new pod, which gets a new
// UID. So the UID is the whole check. An image edited in place on the same
// pod is not caught; this is a test suite, and whoever does that mid-run can
// refresh preflight themselves.
func StaleCapabilityRecord(rec env.ServerInfo, pod *corev1.Pod) string {
	c := rec.Capabilities
	if c.PodUID == "" {
		return fmt.Sprintf("the preflight record of %s/%s predates the pod UID that ties it to a pod; re-run "+
			"preflight with -refresh-preflight", rec.Namespace, rec.Pod)
	}
	if string(pod.UID) != c.PodUID {
		return fmt.Sprintf("%s/%s is pod %s now and preflight read pod %s, so it has been recreated since; "+
			"re-run preflight with -refresh-preflight", rec.Namespace, rec.Pod, pod.UID, c.PodUID)
	}
	return ""
}
