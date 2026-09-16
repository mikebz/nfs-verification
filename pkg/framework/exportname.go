package framework

import (
	"fmt"
	"strings"
	"unicode"
)

// The volume name is the one input a case chooses and the provisioner has to
// carry all the way into an export. Kubernetes checks the name against RFC 1123
// before anything storage-shaped sees it, so the names that reach the driver are
// exactly the ones RFC 1123 permits: lowercase letters, digits, dashes and dots,
// up to 253 characters. Everything here is about those names and what comes back
// out the other side.
//
// The interesting failure is not rejection, it is acceptance with a cut. A
// provisioner that builds an export directory out of the claim name meets
// NAME_MAX at 255 bytes per path component, and a name of 253 characters with
// anything prepended is already past it. If it truncates rather than fails, two
// claims agreeing on their first N characters share one export, and the leak
// looks like an application bug for as long as it takes someone to read a path.
//
// All of it is pure string work over values a cluster produced, which is the
// kind of thing that has to be testable without a cluster.

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

// nameProbeLen is how much of a claim name has to appear in a minted identifier
// before the harness calls that identifier name-derived. The framework's own
// prefix is around thirty characters of case id and run id, so a run of this
// length cannot be a coincidence of some unrelated volume's path.
const nameProbeLen = 32

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

// ExportNaming says how an identifier a provisioner minted relates to the claim
// name it was minted for.
//
// The three answers are different findings. An identifier that carries the name
// whole is a provisioner that handled the boundary. One that carries no trace of
// it is a provisioner that names exports by UID, which is a different design and
// not a defect. One that starts with the name and then stops matching it is the
// defect: the name was used and altered, by a cut at NAME_MAX or by a character
// the driver would not put in a path, and either way two claims that agree up to
// that point land on one export.
type ExportNaming struct {
	// CarriesName is set when the whole claim name appears in the identifier.
	CarriesName bool
	// DivergesAt is how many leading characters of the claim name the identifier
	// carries before it stops matching, when it carries some but not all.
	DivergesAt int
}

// InspectExportName reports how a minted identifier, an export path or a CSI
// volume handle, relates to the claim name.
//
// Absence is the zero value, because a driver is free to name exports by UID.
// Only a partial copy is reported, and only past nameProbeLen, so a path that
// happens to share a few characters with a claim is not called a defect.
func InspectExportName(claim, minted string) ExportNaming {
	if claim == "" || minted == "" {
		return ExportNaming{}
	}
	if strings.Contains(minted, claim) {
		return ExportNaming{CarriesName: true}
	}
	probe := nameProbeLen
	if probe > len(claim) {
		probe = len(claim)
	}
	at := strings.Index(minted, claim[:probe])
	if at < 0 {
		return ExportNaming{}
	}
	n := probe
	for at+n < len(minted) && n < len(claim) && minted[at+n] == claim[n] {
		n++
	}
	return ExportNaming{DivergesAt: n}
}

// CheckExportSyntax rejects a value a provisioner minted that cannot be used as
// the thing it is: an export server, an export path, a volume handle.
//
// Emptiness is the obvious one. The rest is whitespace and control characters,
// which are not a matter of taste here: /etc/exports separates a path from its
// client list with whitespace, a mount command line does the same, and a control
// character in either is nothing a driver can have meant. The field is named in
// the error because the caller checks several and a failure has to say which.
func CheckExportSyntax(field, value string) error {
	if value == "" {
		return fmt.Errorf("the %s is empty, so the volume names no export to mount", field)
	}
	for i, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("the %s %q holds %q at offset %d: whitespace separates the fields of an "+
				"export line and of a mount command, so a value carrying it is a malformed export rather "+
				"than an unusual one", field, value, r, i)
		}
	}
	return nil
}
