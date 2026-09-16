package framework

import (
	"strings"
	"testing"
)

// These three are the whole of PROV-10 that can be checked without a cluster,
// and the case is worth very little if they are wrong. A boundary name that is
// not actually at the boundary tests nothing; a truncation check that never
// fires turns the case green on the defect it exists to find.

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

// TestInspectExportName covers the three answers a minted identifier can give.
//
// Goal: a provisioner that names exports by UID is not a defect, one that
// carries the claim name whole is not a defect, and one that carries the name
// only up to a point is, because two claims agreeing that far then share an
// export. Getting the middle case wrong fails a healthy driver; getting the last
// one wrong passes the leak.
//
// Steps:
//  1. Inspect a UID-named export path against a long claim name.
//  2. Inspect a path that embeds the claim name whole.
//  3. Inspect a path that embeds the name cut at a NAME_MAX-shaped boundary, and
//     check the reported divergence is where the cut is.
//  4. Inspect a path that embeds the name with its first dot replaced, which is
//     the same collision by another route.
//  5. Inspect the degenerate inputs, and a path sharing only a few characters.
func TestInspectExportName(t *testing.T) {
	claim, err := BoundaryVolumeName("nfsv-prov-10-20260916-010203-", MaxObjectNameLength)
	if err != nil {
		t.Fatalf("building the claim name under test: %v", err)
	}

	if got := InspectExportName(claim, "/export/pvc-3f2b7d2e-6a41-4f0f-9a9f-2c1f0a8b7d55"); got != (ExportNaming{}) {
		t.Errorf("a UID-named export was read as name-derived: %+v", got)
	}
	if got := InspectExportName(claim, "/export/default-"+claim+"-pvc-3f2b7d2e"); !got.CarriesName {
		t.Errorf("an export carrying the whole claim name was not read as carrying it: %+v", got)
	}
	// 255 bytes is NAME_MAX on every filesystem this runs over, and a 253
	// character name with anything prepended is already past it.
	const cut = 200
	got := InspectExportName(claim, "/export/"+claim[:cut])
	if got.CarriesName {
		t.Fatalf("a cut copy of the claim name was read as the whole name")
	}
	if got.DivergesAt != cut {
		t.Errorf("the divergence was reported at %d characters, want %d; the failure message quotes "+
			"this number", got.DivergesAt, cut)
	}

	// A driver that will not put a dot in a path and replaces it collides two
	// claims differing only in that character, which is the same finding.
	dot := strings.Index(claim, ".")
	if dot < nameProbeLen {
		t.Fatalf("the boundary name's first dot is at %d, inside the %d-character probe, so this "+
			"case is not testing what it says", dot, nameProbeLen)
	}
	substituted := InspectExportName(claim, "/export/"+strings.Replace(claim, ".", "-", 1))
	if substituted.CarriesName || substituted.DivergesAt != dot {
		t.Errorf("a substituted character was read as %+v, want a divergence at %d", substituted, dot)
	}

	for _, minted := range []string{"", "/export/a1-b2", "/export/pvc"} {
		if got := InspectExportName(claim, minted); got != (ExportNaming{}) {
			t.Errorf("export path %q was read as name-derived: %+v", minted, got)
		}
	}
	if got := InspectExportName("", "/export/anything"); got != (ExportNaming{}) {
		t.Errorf("an empty claim name produced a derivation: %+v", got)
	}
}

// TestCheckExportSyntax covers the shapes an export value must not have.
//
// Goal: whitespace and control characters in a server or a path are a malformed
// export, not an unusual one, because both /etc/exports and a mount command line
// are whitespace-separated. The check runs over values a cluster produced, so the
// only place it can be exercised is here.
//
// Steps:
//  1. Accept the ordinary shapes: an address, a hostname, an export path, a
//     handle with the separators drivers use.
//  2. Reject empty, and reject each of newline, carriage return, tab and space.
//  3. Assert the failure names the field, since the caller checks several.
func TestCheckExportSyntax(t *testing.T) {
	for _, value := range []string{
		"10.0.0.5",
		"nfs.default.svc.cluster.local",
		"/export/pvc-3f2b7d2e-6a41-4f0f-9a9f-2c1f0a8b7d55",
		"10.0.0.5#/export#pvc-123##",
	} {
		if err := CheckExportSyntax("export path", value); err != nil {
			t.Errorf("rejected an ordinary value %q: %v", value, err)
		}
	}

	if err := CheckExportSyntax("export path", ""); err == nil {
		t.Error("an empty export path was accepted")
	}
	for _, value := range []string{"/export/a\nb", "/export/a\rb", "/export/a\tb", "/export/a b", "/export/a\x00b"} {
		err := CheckExportSyntax("export server", value)
		if err == nil {
			t.Errorf("accepted %q, which cannot survive an export line or a mount command", value)
			continue
		}
		if !strings.Contains(err.Error(), "export server") {
			t.Errorf("the failure does not name the field it came from: %v", err)
		}
	}
}
