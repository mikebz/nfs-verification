// Package e2e implements the case table in Section 3 of the test plan. Each
// test names its plan case in the comment above it. Nothing runs until
// preflight has passed, and nothing asserts more than NFSv4.1 guarantees.
package e2e

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/mikebz/nfs-verification/pkg/env"
	"github.com/mikebz/nfs-verification/pkg/framework"
	"github.com/mikebz/nfs-verification/pkg/preflight"
)

// TestMain is the suite entry point rather than a test. Nothing runs until
// preflight has passed, because cases run against an environment that failed
// discovery produce failures that cost days to route and prove nothing.
//
// Steps:
//  1. Parse and finalize the flags.
//  2. Use an environment record named with -env-file, if there is one.
//  3. Otherwise reuse a recent preflight result cached for this kubeconfig
//     context, since preflight answers the same way against an unchanged
//     cluster.
//  4. Otherwise run preflight, and exit non-zero with the specific reason if
//     it fails.
//  5. Write the run's environment record, or check it names the same cluster
//     when the run id already has one, and stop if it names another.
//  6. Publish the result to the fixtures and run the cases.
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
	e, caps, err := discover(c)
	if err != nil {
		return err
	}
	// On every path, not only a fresh preflight: a run that reused a cached
	// result would otherwise leave no record, and a later invocation under the
	// same run id against another cluster would write one unchecked.
	if err := framework.WriteRunEnvironment(e); err != nil {
		return fmt.Errorf("recording the run's environment: %w", err)
	}
	framework.SetSuite(c, e, caps)
	return nil
}

// discover returns the environment the suite runs against, from the cheapest
// source that is trustworthy: an explicit file, then a recent cached preflight
// for this context, then a fresh preflight.
func discover(c *framework.Client) (*env.Environment, framework.Capabilities, error) {
	// An explicit file wins over everything.
	if path := framework.Cfg().EnvFile; path != "" {
		e, err := env.Load(path)
		if err != nil {
			return nil, framework.Capabilities{}, fmt.Errorf("loading %s: %w", path, err)
		}
		return e, framework.CapabilitiesFromMap(e.Capabilities), nil
	}
	// Preflight against an unchanged cluster answers the same way every time,
	// so a recent result for this context is reused rather than repeated.
	if res, path := preflight.Cached(c); res != nil {
		log.Printf("reusing the preflight result from %s (discovered %s ago; -refresh-preflight to redo it)",
			path, time.Since(res.Env.Timestamp).Round(time.Second))
		return res.Env, res.Caps, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := preflight.Run(ctx)
	if err != nil {
		return nil, framework.Capabilities{}, err
	}
	return res.Env, res.Caps, nil
}
