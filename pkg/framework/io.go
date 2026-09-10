package framework

import (
	"context"
	"fmt"
)

// WriteFile writes size bytes of deterministic, seeded content at path and
// returns its sha256. Deterministic content matters: a checksum mismatch after
// a failover has to be reproducible to be filed.
func (f *Framework) WriteFile(ctx context.Context, pod, path string, sizeBytes int, seed string) (string, error) {
	script := fmt.Sprintf(
		`set -e; mkdir -p "$(dirname %[1]s)"; `+
			`yes %[3]s | head -c %[2]d > %[1]s; `+
			`sync; sha256sum %[1]s | cut -d' ' -f1`,
		shellQuote(path), sizeBytes, shellQuote(seed))
	return f.C.MustSh(ctx, Namespace, f.Name(pod), "main", script)
}

// Sha256 returns the checksum of a file as the pod sees it.
func (f *Framework) Sha256(ctx context.Context, pod, path string) (string, error) {
	return f.C.MustSh(ctx, Namespace, f.Name(pod), "main",
		fmt.Sprintf("sha256sum %s | cut -d' ' -f1", shellQuote(path)))
}

// Quote exposes shell quoting to test packages building their own scripts.
func Quote(s string) string { return shellQuote(s) }
