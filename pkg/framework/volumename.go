package framework

import (
	"fmt"
	"strings"
)

// The claim name is the one input PROV-10 chooses. Kubernetes checks it against
// RFC 1123 before any storage component sees it, so the names that reach a
// provisioner at all are exactly the ones RFC 1123 permits: lowercase letters,
// digits, dashes and dots, up to 253 characters. A name outside that set is the
// apiserver's business and not this suite's; a name at the edge of it is the one
// worth driving through a real provisioner.
//
// Building that name is pure string work, and it has to come out valid on the
// first try: a name Kubernetes rejects fails the case minutes into a run with a
// message about the rendered object rather than about the generator that made
// it. So it lives here, where a unit test can hold it, rather than in the case.

// MaxObjectNameLength is the longest name Kubernetes will store for an object.
const MaxObjectNameLength = 253

// boundaryNameUnit is the repeating body of a boundary name: a label of letters
// and digits around an interior dash, then the dot that starts the next label.
//
// Dots and dashes are the two characters RFC 1123 allows that a provisioner then
// has to carry into a path, a config file or a command line, so a name built to
// test the driver is built out of them rather than out of one repeated letter.
const boundaryNameUnit = "a1-b2c3."

// minBoundaryPad is the least a boundary name may contribute beyond the run
// prefix. Two units, so the part under test still holds a dot and a dash after
// the tail is trimmed.
const minBoundaryPad = 2 * len(boundaryNameUnit)

// BoundaryVolumeName builds the longest name Kubernetes will accept, out of the
// characters it will accept, on top of a prefix the caller already owns.
//
// It exists for PROV-10, which drives a name at the boundary through the
// provisioner rather than at the apiserver. The name is prefix-first so it stays
// attributable to the run that made it, and the harness's own Name is idempotent
// over a name that already carries the prefix.
//
// The result is a valid RFC 1123 subdomain of exactly length characters, ending
// on a letter or a digit because a name may not end on a dot or a dash and the
// cut lands wherever the arithmetic puts it.
func BoundaryVolumeName(prefix string, length int) (string, error) {
	if length > MaxObjectNameLength {
		return "", fmt.Errorf("a boundary name of %d characters is longer than the %d Kubernetes stores",
			length, MaxObjectNameLength)
	}
	if pad := length - len(prefix); pad < minBoundaryPad {
		return "", fmt.Errorf("the run prefix %q is %d characters, which leaves %d of a %d-character name "+
			"for the part under test and at least %d is needed to carry a dot and a dash; shorten -run-id",
			prefix, len(prefix), pad, length, minBoundaryPad)
	}
	var b strings.Builder
	b.Grow(length + len(boundaryNameUnit))
	b.WriteString(prefix)
	for b.Len() < length {
		b.WriteString(boundaryNameUnit)
	}
	name := strings.TrimRight(b.String()[:length], ".-")
	name += strings.Repeat("z", length-len(name))
	if err := CheckObjectName("boundary claim", name); err != nil {
		return "", err
	}
	return name, nil
}
