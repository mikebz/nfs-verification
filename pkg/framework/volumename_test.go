package framework

import (
	"strings"
	"testing"
)

// The boundary name is the whole of PROV-10 that can be checked without a
// cluster, and a name that is not actually at the boundary tests nothing while
// still passing. A generator that produces a name Kubernetes rejects is worse:
// the case dies minutes into a run with a message about the rendered object.

// TestBoundaryVolumeName covers the name PROV-10 sends through the provisioner.
//
// Goal: the name is at the boundary, is a name Kubernetes will store, and is
// built out of the characters that reach the driver rather than one repeated
// letter.
//
// Steps:
//  1. Build a name at every length from the minimum up to the Kubernetes limit.
//  2. Assert each is exactly the length asked for and passes the same check
//     CreatePVC runs before it creates anything.
//  3. Assert each carries the prefix, a dot and a dash, and ends on a letter or
//     a digit, which is where a naive pad lands wrong.
//  4. Assert a prefix that leaves no room is an error naming -run-id, rather
//     than a short name that quietly stops being a boundary.
func TestBoundaryVolumeName(t *testing.T) {
	const prefix = "nfsv-prov-10-20260916-010203-"
	for length := len(prefix) + minBoundaryPad; length <= MaxObjectNameLength; length++ {
		name, err := BoundaryVolumeName(prefix, length)
		if err != nil {
			t.Fatalf("building a %d-character boundary name: %v", length, err)
		}
		if len(name) != length {
			t.Fatalf("boundary name is %d characters, want %d: %q", len(name), length, name)
		}
		if err := CheckObjectName("claim", name); err != nil {
			t.Fatalf("Kubernetes would reject the %d-character boundary name: %v", length, err)
		}
		if !strings.HasPrefix(name, prefix) {
			t.Fatalf("boundary name %q does not carry the run prefix, so it is not attributable", name)
		}
		if !strings.Contains(name[len(prefix):], ".") || !strings.Contains(name[len(prefix):], "-") {
			t.Fatalf("the part of %q under test carries no dot or no dash, so the character set "+
				"the provisioner has to handle is not being exercised", name)
		}
	}

	if _, err := BoundaryVolumeName(strings.Repeat("p", MaxObjectNameLength-1), MaxObjectNameLength); err == nil {
		t.Error("a prefix leaving no room produced a name rather than an error")
	} else if !strings.Contains(err.Error(), "run-id") {
		t.Errorf("the failure does not say what to shorten: %v", err)
	}
	if _, err := BoundaryVolumeName("nfsv-x-", MaxObjectNameLength+1); err == nil {
		t.Error("a length past the Kubernetes limit produced a name rather than an error")
	}
}
