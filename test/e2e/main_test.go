// Package e2e implements the case table in Section 3 of the test plan. Each
// test names its plan case in the comment above it. Nothing runs until
// preflight has passed, and nothing asserts more than NFSv4.1 guarantees.
package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/mikebz/nfs-verification/pkg/env"
	"github.com/mikebz/nfs-verification/pkg/framework"
	"github.com/mikebz/nfs-verification/pkg/preflight"
)

func TestMain(m *testing.M) {
	flag.Parse()
	if err := framework.FinalizeFlags(); err != nil {
		fmt.Fprintf(os.Stderr, "bad flags: %v\n", err)
		os.Exit(2)
	}
	if err := setup(); err != nil {
		// A preflight failure stops the suite. Running cases against an
		// environment that failed discovery produces failures that cost days to
		// route and prove nothing.
		fmt.Fprintf(os.Stderr, "PREFLIGHT FAILED: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func setup() error {
	c, err := framework.NewClient()
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}
	if path := framework.Cfg().EnvFile; path != "" {
		e, err := env.Load(path)
		if err != nil {
			return fmt.Errorf("loading %s: %w", path, err)
		}
		framework.SetSuite(c, e, framework.CapabilitiesFromMap(e.Capabilities))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := preflight.Run(ctx)
	if err != nil {
		return err
	}
	framework.SetSuite(c, res.Env, res.Caps)
	return nil
}
