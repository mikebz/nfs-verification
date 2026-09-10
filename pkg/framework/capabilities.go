package framework

// Capabilities is what the cluster in front of us can actually do. Every case
// is skippable by capability, never by platform name.
type Capabilities struct {
	// MultiNode: at least two schedulable workers.
	MultiNode bool
	// NodeAgent: the privileged DaemonSet scheduled, so /proc/mounts, dmesg and
	// process signals are available.
	NodeAgent bool
	// CanStopNode: a platform implementation can hard-stop and restart a node.
	CanStopNode bool
	// CanNetworkPolicy: the CNI enforces NetworkPolicy, verified by probe.
	CanNetworkPolicy bool
	// CanSnapshot: VolumeSnapshot CRDs are served.
	CanSnapshot bool
	// CanExpand: the StorageClass advertises volume expansion.
	CanExpand bool
	// DualStack: both IP families present in the cluster.
	DualStack bool
	// SharedServer: server fan-out is greater than one, so exports share a
	// process and noisy-neighbour cases apply.
	SharedServer bool
	// DelegationsEnabled gates the delegation recall case.
	DelegationsEnabled bool
	// IndependentVersions gates the SKEW cases.
	IndependentVersions bool
	// NodeLossConfigured: tolerationSeconds and the out-of-service taint are
	// both in place, so the ungraceful node loss SLO is reachable at all.
	NodeLossConfigured bool
}

// AsMap renders capabilities for environment.json.
func (c Capabilities) AsMap() map[string]bool {
	return map[string]bool{
		"multiNode":           c.MultiNode,
		"nodeAgent":           c.NodeAgent,
		"canStopNode":         c.CanStopNode,
		"canNetworkPolicy":    c.CanNetworkPolicy,
		"canSnapshot":         c.CanSnapshot,
		"canExpand":           c.CanExpand,
		"dualStack":           c.DualStack,
		"sharedServer":        c.SharedServer,
		"delegationsEnabled":  c.DelegationsEnabled,
		"independentVersions": c.IndependentVersions,
		"nodeLossConfigured":  c.NodeLossConfigured,
	}
}

// CapabilitiesFromMap rebuilds capabilities from a stored environment record,
// so that a rerun with -env-file gates exactly as the original run did.
func CapabilitiesFromMap(m map[string]bool) Capabilities {
	return Capabilities{
		MultiNode:           m["multiNode"],
		NodeAgent:           m["nodeAgent"],
		CanStopNode:         m["canStopNode"],
		CanNetworkPolicy:    m["canNetworkPolicy"],
		CanSnapshot:         m["canSnapshot"],
		CanExpand:           m["canExpand"],
		DualStack:           m["dualStack"],
		SharedServer:        m["sharedServer"],
		DelegationsEnabled:  m["delegationsEnabled"],
		IndependentVersions: m["independentVersions"],
		NodeLossConfigured:  m["nodeLossConfigured"],
	}
}
