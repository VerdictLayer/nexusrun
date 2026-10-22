package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/verdictlayer/nexusrun/internal/fleet"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
)

func fleetAdmin(repository string) (*fleet.Client, error) {
	c, err := fleet.NewClient(repository, "")
	if err != nil {
		return nil, err
	}
	host := strings.SplitN(repository, "/", 2)[0]
	c.Token = unit.RegistryToken(host)
	if c.Token == "" {
		return nil, errors.New("sign in with nexus login before managing the fleet")
	}
	return c, nil
}

func newFleetCmd() *cobra.Command {
	c := &cobra.Command{Use: "fleet", Short: "Enroll hosts and manage test-gated agent releases"}
	c.AddCommand(newFleetJoinCmd(), newFleetSyncCmd(false), newFleetSyncCmd(true), newFleetRunCmd(), newFleetStatusCmd(), newFleetListCmd(), newFleetReleaseCmd(false), newFleetReleaseCmd(true), newFleetPauseCmd(), newFleetRemoveCmd())
	return c
}

func newFleetJoinCmd() *cobra.Command {
	var name, channel string
	c := &cobra.Command{Use: "join <registry/org/repository>", Short: "Enroll this Linux amd64 host for one repository and release channel", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !fleet.ValidName(name) || !fleet.ValidChannel(channel) {
			return errors.New("a valid --name and --channel are required")
		}
		hw := fleet.HostHardware()
		if hw.OS != "linux" || hw.Arch != "amd64" {
			return errors.New("fleet enrollment requires Linux amd64")
		}
		s, err := store.Open()
		if err != nil {
			return err
		}
		unlock, err := fleet.Lock(s.Root)
		if err != nil {
			return err
		}
		defer unlock()
		if _, err = fleet.LoadEnrollment(s.Root); !errors.Is(err, os.ErrNotExist) {
			return errors.New("this NexusRun home is already enrolled or has invalid enrollment state")
		}
		client, err := fleetAdmin(args[0])
		if err != nil {
			return err
		}
		var enrolled struct {
			HostID string `json:"hostID"`
			Token  string `json:"token"`
		}
		if err = client.Admin(cmd.Context(), "POST", "enroll", map[string]any{"name": name, "repository": client.Repository, "channel": channel, "hardware": hw}, &enrolled); err != nil {
			return err
		}
		if err = fleet.SaveEnrollment(s.Root, fleet.Enrollment{HostID: enrolled.HostID, Name: name, Repository: args[0], Channel: channel, Token: enrolled.Token}); err != nil {
			return fmt.Errorf("host enrolled but local credentials could not be saved; remove host %s in Cloud before retrying", enrolled.HostID)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Enrolled %s (%s) in %s\nNext: nexus fleet sync\n", name, enrolled.HostID, channel)
		return nil
	}}
	c.Flags().StringVar(&name, "name", "", "unique host label, without paths or personal information")
	c.Flags().StringVar(&channel, "channel", "production", "release channel")
	_ = c.MarkFlagRequired("name")
	return c
}

func newFleetSyncCmd(watch bool) *cobra.Command {
	name := "sync"
	description := "Check for an update, test it locally, then atomically activate it"
	if watch {
		name = "watch"
		description = "Poll outbound for fleet updates until interrupted"
	}
	var interval, timeout time.Duration
	c := &cobra.Command{Use: name, Short: description, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if timeout < time.Second || timeout > time.Hour || interval < 5*time.Second {
			return errors.New("timeout must be 1s-1h and interval at least 5s")
		}
		s, err := store.Open()
		if err != nil {
			return err
		}
		e, err := fleet.LoadEnrollment(s.Root)
		if err != nil {
			return errors.New("enroll this host with nexus fleet join first")
		}
		client, err := fleet.NewClient(e.Repository, e.Token)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer stop()
		for {
			op, cancel := context.WithTimeout(ctx, timeout)
			result, syncErr := fleet.Sync(op, s.Root, fleet.HostHardware(), client, fleet.NativePrepare(s.Root))
			cancel()
			if err = json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
				return err
			}
			if !watch {
				return syncErr
			}
			if syncErr != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), syncErr)
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
	}}
	c.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "maximum time to download and test one candidate")
	c.Flags().DurationVar(&interval, "interval", 30*time.Second, "poll interval for watch")
	return c
}

func newFleetRunCmd() *cobra.Command {
	var prompt string
	var tokens int
	c := &cobra.Command{Use: "run", Short: "Run the active local fleet release through its exact CPU runtime", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := store.Open()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer stop()
		res, err := fleet.Run(ctx, s.Root, prompt, tokens)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), res.Text)
		fmt.Fprintf(cmd.ErrOrStderr(), "%s on CPU · %d tokens\n", res.Backend, res.TokensOut)
		return nil
	}}
	c.Flags().StringVarP(&prompt, "prompt", "p", "", "question to send to the active agent")
	c.Flags().IntVarP(&tokens, "max-tokens", "n", 256, "maximum output tokens")
	_ = c.MarkFlagRequired("prompt")
	return c
}

func newFleetStatusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show local enrollment and active release without contacting Cloud", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := store.Open()
		if err != nil {
			return err
		}
		e, err := fleet.LoadEnrollment(s.Root)
		if err != nil {
			return errors.New("this NexusRun home has no readable fleet enrollment")
		}
		state, err := fleet.LoadState(s.Root)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"hostID": e.HostID, "name": e.Name, "repository": e.Repository, "channel": e.Channel, "active": state.Active, "previous": state.Previous, "lastCode": state.LastCode, "reportPending": state.Pending != nil})
	}}
}

func fleetOrgClient(ref string) (*fleet.Client, error) {
	if strings.Count(ref, "/") != 1 {
		return nil, errors.New("organization must be registry/organization")
	}
	return fleetAdmin(ref + "/fleet")
}

func newFleetListCmd() *cobra.Command {
	return &cobra.Command{Use: "list <registry/org>", Short: "List enrolled hosts, release history and aggregate results", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := fleetOrgClient(args[0])
		if err != nil {
			return err
		}
		var result json.RawMessage
		if err = c.Admin(cmd.Context(), "GET", "status", nil, &result); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(result))
		return nil
	}}
}

func newFleetReleaseCmd(rollback bool) *cobra.Command {
	var channel, digest, runtimeID string
	var canaries int
	verb := "release"
	description := "Publish an immutable candidate, beginning with enrolled canary hosts"
	if rollback {
		verb = "rollback"
		description = "Create a test-gated rollout of the previous active release"
	}
	c := &cobra.Command{Use: verb + " <registry/org/repository>", Short: description, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !fleet.ValidChannel(channel) || canaries < 1 || canaries > 1000 {
			return errors.New("valid channel and 1-1000 canaries required")
		}
		if !rollback && (!fleet.ValidDigest(digest) || !fleet.ValidRuntime(runtimeID)) {
			return errors.New("an immutable --digest and Linux amd64 CPU --runtime ID are required")
		}
		client, err := fleetAdmin(args[0])
		if err != nil {
			return err
		}
		body := map[string]any{"repository": client.Repository, "channel": channel, "canaryCount": canaries}
		if !rollback {
			body["digest"] = digest
			body["runtimeID"] = runtimeID
		}
		var result json.RawMessage
		action := "releases"
		if rollback {
			action = "rollback"
		}
		if err = client.Admin(cmd.Context(), "POST", action, body, &result); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(result))
		return nil
	}}
	c.Flags().StringVar(&channel, "channel", "production", "release channel")
	c.Flags().IntVar(&canaries, "canaries", 1, "number of enrolled hosts that must activate before wider rollout")
	if !rollback {
		c.Flags().StringVar(&digest, "digest", "", "exact pushed artifact SHA-256 digest")
		c.Flags().StringVar(&runtimeID, "runtime", "", "exact managed CPU runtime ID, preinstalled on the hosts")
		_ = c.MarkFlagRequired("digest")
		_ = c.MarkFlagRequired("runtime")
	}
	return c
}

func newFleetPauseCmd() *cobra.Command {
	return &cobra.Command{Use: "pause <registry/org> <release-id>", Short: "Stop further activation of a release", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := fleetOrgClient(args[0])
		if err != nil {
			return err
		}
		return c.Admin(cmd.Context(), "POST", "releases/"+args[1]+"/pause", map[string]any{}, nil)
	}}
}
func newFleetRemoveCmd() *cobra.Command {
	return &cobra.Command{Use: "remove <registry/org> <host-id>", Short: "Revoke a host's update and repository access", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := fleetOrgClient(args[0])
		if err != nil {
			return err
		}
		return c.Admin(cmd.Context(), "DELETE", "hosts/"+args[1], nil, nil)
	}}
}
