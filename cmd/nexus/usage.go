package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/verdictlayer/nexusrun/internal/unit"
	"github.com/verdictlayer/nexusrun/internal/usage"
)

func newUsageCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "usage [registry]",
		Short: "Show your NexusRun Cloud plan, storage and bandwidth",
		Long: `Show the plan, storage and this month's pull bandwidth for the account you
are signed in to (see 'nexus login').

Example:
  nexus usage
  nexus usage --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := hostArg(args)
			token := unit.RegistryToken(host)
			if token == "" {
				return fmt.Errorf("not logged in to %s; run `%s login`", host, binName)
			}
			r, err := usage.Fetch(cmd.Context(), nil, baseURL(host), token)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(r)
			}
			printUsage(host, r)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the raw report as JSON")
	return cmd
}

func printUsage(host string, r *usage.Report) {
	fmt.Printf("%s  ·  %s plan  ·  org %s\n", host, r.Plan, r.Org)
	if r.ReadOnly {
		fmt.Println("  read-only: pulls work, pushes are refused until a plan is chosen")
	}
	if r.TrialEnds != nil {
		fmt.Printf("  trial ends: %s\n", strings.SplitN(*r.TrialEnds, " ", 2)[0])
	}
	fmt.Printf("  storage:   %s of %s used (%s free)\n", humanSize(r.Storage.Used), humanSize(r.Storage.Limit), humanSize(r.Storage.Remaining))
	b := r.Bandwidth
	switch {
	case b.Included == nil:
		fmt.Printf("  bandwidth: %s pulled in %s (no limit)\n", humanSize(b.Used), b.Period)
	case b.Overage > 0 && b.OverageBilled:
		fmt.Printf("  bandwidth: %s of %s pulled in %s (%s over, billed as overage)\n", humanSize(b.Used), humanSize(*b.Included), b.Period, humanSize(b.Overage))
	case b.Overage > 0:
		fmt.Printf("  bandwidth: %s of %s pulled in %s (limit reached)\n", humanSize(b.Used), humanSize(*b.Included), b.Period)
	default:
		fmt.Printf("  bandwidth: %s of %s pulled in %s (%s left)\n", humanSize(b.Used), humanSize(*b.Included), b.Period, humanSize(*b.Remaining))
	}
	if r.HostLimit != nil {
		fmt.Printf("  hosts:     up to %d\n", *r.HostLimit)
	}
}

// preflightPush shows remaining storage before a push to a NexusRun Cloud
// registry and stops early when the account is read-only. Anything that stops
// it from getting an answer is ignored: the push itself reports real errors.
func preflightPush(ctx context.Context, target string) error {
	host := target
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	token := unit.RegistryToken(host)
	if !strings.HasPrefix(token, "nxr_pat_") {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := usage.Fetch(ctx, nil, baseURL(host), token)
	if err != nil {
		if errors.Is(err, usage.ErrUnauthorized) {
			return err
		}
		return nil
	}
	if r.ReadOnly {
		return fmt.Errorf("%s is read-only (%s plan): pulls work, pushes are refused until you choose a plan in the dashboard", host, r.Plan)
	}
	logf("storage: %s of %s used, %s free", humanSize(r.Storage.Used), humanSize(r.Storage.Limit), humanSize(r.Storage.Remaining))
	return nil
}
