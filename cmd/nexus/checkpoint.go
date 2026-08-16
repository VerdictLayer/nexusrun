package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/verdictlayer/nexusrun/internal/checkpoint"
	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/session"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
)

// --- session --------------------------------------------------------------

func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage the conversations agents remember",
		Long: `A session is an agent's continuing conversation, kept across runs.

  nexus run my-agent -s work -p "remember: the deploy key is in vault"
  nexus run my-agent -s work -p "where did I say the deploy key was?"

Without --session a run is one-shot and nothing is kept, which is still
the default.`,
	}
	cmd.AddCommand(newSessionListCmd(), newSessionShowCmd(), newSessionRemoveCmd())
	return cmd
}

func newSessionListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List sessions, most recent first",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			list, err := session.List(s)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(list)
			}
			if len(list) == 0 {
				fmt.Printf("No sessions yet. Start one:\n  %s run <unit> -s <name> -p \"…\"\n", binName)
				return nil
			}
			nameW, unitW := len("NAME"), len("UNIT")
			for _, sess := range list {
				nameW = max(nameW, len(sess.Name))
				unitW = max(unitW, len(sess.Unit))
			}
			row := fmt.Sprintf("  %%-%ds  %%-%ds  %%6s  %%8s  %%-18s  %%s\n", nameW, unitW)
			fmt.Printf(row, "NAME", "UNIT", "TURNS", "MESSAGES", "UPDATED", "LAST PROMPT")
			for _, sess := range list {
				fmt.Printf(row, sess.Name, sess.Unit,
					fmt.Sprint(sess.Turns), fmt.Sprint(len(sess.Messages)),
					sess.Updated.Local().Format("2006-01-02 15:04"), sess.Summary())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

func newSessionShowCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Print a session's transcript",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			sess, err := session.Load(s, args[0])
			if err != nil {
				return err
			}
			if sess == nil {
				return fmt.Errorf("no session named %q", args[0])
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(sess)
			}
			fmt.Printf("Session:  %s\nUnit:     %s\n", sess.Name, sess.Unit)
			if sess.Model != "" {
				fmt.Printf("Model:    %s\n", sess.Model)
			}
			fmt.Printf("Turns:    %d over %d message(s)\nUpdated:  %s\n\n",
				sess.Turns, len(sess.Messages), sess.Updated.Local().Format(time.RFC3339))
			printTranscript(sess.Messages)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

// printTranscript renders a conversation, including the tool exchanges.
// Those are the part worth seeing when an agent went wrong, and they are
// invisible in the final answer.
func printTranscript(msgs []engine.Message) {
	for _, msg := range msgs {
		switch msg.Role {
		case engine.RoleSystem:
			fmt.Printf("── system ──\n%s\n\n", strings.TrimSpace(msg.Content))
		case engine.RoleUser:
			fmt.Printf("── you ──\n%s\n\n", strings.TrimSpace(msg.Content))
		case engine.RoleAssistant:
			fmt.Printf("── agent ──\n")
			if txt := strings.TrimSpace(msg.Content); txt != "" {
				fmt.Printf("%s\n", txt)
			}
			for _, c := range msg.ToolCalls {
				fmt.Printf("  → calls %s\n", c.Summary())
			}
			fmt.Println()
		case engine.RoleTool:
			fmt.Printf("── %s returned ──\n%s\n\n", msg.Name, strings.TrimSpace(msg.Content))
		}
	}
}

func newSessionRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a session",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			if err := session.Remove(s, args[0]); err != nil {
				return err
			}
			fmt.Printf("Removed session %s.\n", args[0])
			return nil
		},
	}
}

// --- checkpoint -----------------------------------------------------------

func newCheckpointCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checkpoint",
		Short: "Move an agent's state between machines",
		Long: `A checkpoint is an agent's conversation and memory in one portable
file, so a robot that reboots, or a session that has to continue on
another machine, picks up where it left off.

  nexus checkpoint save work -o work.state.nx
  nexus run my-agent --restore work.state.nx -p "carry on"

The KV cache is deliberately not captured. llama.cpp's cache is not
portable across versions, quantizations, or architectures, and this
runtime drives backends as subprocesses, so there is no cache handle to
take. Restoring a conversation costs one prompt re-ingest on the first
turn and is correct everywhere.`,
	}
	cmd.AddCommand(
		newCheckpointSaveCmd(),
		newCheckpointListCmd(),
		newCheckpointInspectCmd(),
		newCheckpointRemoveCmd(),
	)
	return cmd
}

func newCheckpointSaveCmd() *cobra.Command {
	var out string
	var encrypt, seal bool
	cmd := &cobra.Command{
		Use:   "save <session>",
		Short: "Write a session to a portable checkpoint",
		Long: `Save writes a session's conversation and memory to a .state.nx file.

  --encrypt  seal the whole archive under NEXUS_STATE_KEY
  --seal     embed the model weights, making the file self-contained for
             an air-gapped machine that cannot fetch them (gigabytes)

With no -o the checkpoint goes to the store, where "checkpoint list" finds
it. Use "-o -" to write to stdout.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			sess, err := session.Load(s, args[0])
			if err != nil {
				return err
			}
			if sess == nil {
				return fmt.Errorf("no session named %q — `%s session ls` shows what exists", args[0], binName)
			}

			opts := checkpoint.SaveOptions{Encrypt: encrypt, Seal: seal, Progress: logf}
			if seal {
				path, rerr := resolveSessionModel(cmd.Context(), s, sess)
				if rerr != nil {
					return rerr
				}
				opts.ModelPath = path
			}

			var w *os.File
			dest := out
			switch {
			case out == "-":
				w = os.Stdout
			default:
				if dest == "" {
					dir := checkpoint.Dir(s)
					if err := os.MkdirAll(dir, 0o755); err != nil {
						return err
					}
					dest = filepath.Join(dir, sess.Name+"-"+
						time.Now().UTC().Format("20060102T150405Z")+checkpoint.Ext)
				}
				// 0600: a transcript is usually the most sensitive artifact
				// a machine holds.
				w, err = os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}
				defer w.Close()
			}

			man, err := checkpoint.Save(w, sess, opts)
			if err != nil {
				return err
			}
			if out == "-" {
				return nil
			}
			info, _ := os.Stat(dest)
			fmt.Fprintf(os.Stderr, "Wrote %s (%s on disk)\n", dest, humanSize(info.Size()))
			fmt.Fprintf(os.Stderr, "  %d message(s), %d turn(s)", man.Session.Messages, man.Session.Turns)
			if man.Encryption != "" {
				fmt.Fprintf(os.Stderr, ", encrypted")
			}
			if man.Model.Sealed {
				fmt.Fprintf(os.Stderr, ", weights embedded")
			}
			fmt.Fprintf(os.Stderr, "\n  restore: %s run <unit> --restore %s\n", binName, dest)
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "write here instead of the store (- for stdout)")
	cmd.Flags().BoolVar(&encrypt, "encrypt", false, "encrypt with NEXUS_STATE_KEY")
	cmd.Flags().BoolVar(&seal, "seal", false, "embed the model weights for air-gapped transfer")
	return cmd
}

// resolveSessionModel finds the weights to seal into a checkpoint.
func resolveSessionModel(ctx context.Context, s *store.Store, sess *session.Session) (string, error) {
	source := sess.Model
	if source == "" {
		// The session did not record one; fall back to the unit's own.
		m, _, err := unit.Resolve(ctx, s, sess.Unit)
		if err != nil {
			return "", fmt.Errorf("--seal needs the model, and %s could not be resolved: %w", sess.Unit, err)
		}
		if len(m.Models) == 0 || m.Models[0].Source == "" {
			return "", fmt.Errorf("--seal needs a concrete model, but %s does not name one", sess.Unit)
		}
		source = m.Models[0].Source
	}
	resolved, err := s.Resolve(source, "", nil)
	if err != nil {
		return "", fmt.Errorf("--seal: %w", err)
	}
	return resolved.Path, nil
}

func newCheckpointListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "list [session]",
		Aliases: []string{"ls"},
		Short:   "List saved checkpoints",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			list, err := checkpoint.List(s, name)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(list)
			}
			if len(list) == 0 {
				fmt.Printf("No checkpoints. Save one:\n  %s checkpoint save <session>\n", binName)
				return nil
			}
			fmt.Printf("%-40s %-20s %8s %10s  %s\n", "FILE", "SESSION", "MESSAGES", "SIZE", "SAVED")
			for _, c := range list {
				sessName, msgs := "—", "—"
				if c.Manifest != nil {
					sessName = c.Manifest.Session.Name
					msgs = fmt.Sprint(c.Manifest.Session.Messages)
				} else {
					sessName = "(unreadable)"
				}
				fmt.Printf("%-40s %-20s %8s %10s  %s\n",
					filepath.Base(c.Path), sessName, msgs, humanSize(c.Size),
					c.Modified.Local().Format("2006-01-02 15:04"))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

func newCheckpointInspectCmd() *cobra.Command {
	var jsonOut, transcript bool
	cmd := &cobra.Command{
		Use:   "inspect <file>",
		Short: "Show what a checkpoint holds, without restoring it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()

			res, err := checkpoint.Load(f, checkpoint.LoadOptions{
				MetadataOnly: !transcript, Progress: logf,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				out := map[string]any{"manifest": res.Manifest}
				if transcript {
					out["messages"] = res.Session.Messages
				}
				return enc.Encode(out)
			}
			fmt.Print(res.Manifest.String())
			if transcript {
				fmt.Println()
				printTranscript(res.Session.Messages)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	cmd.Flags().BoolVar(&transcript, "transcript", false, "also print the conversation")
	return cmd
}

func newCheckpointRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <file>",
		Aliases: []string{"rm"},
		Short:   "Delete a checkpoint from the store",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			// A bare name refers to the store; a path is taken literally,
			// so this never deletes outside the store by accident.
			path := args[0]
			if !strings.ContainsRune(path, os.PathSeparator) {
				if !strings.HasSuffix(path, checkpoint.Ext) {
					path += checkpoint.Ext
				}
				path = filepath.Join(checkpoint.Dir(s), path)
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			fmt.Printf("Removed %s\n", path)
			return nil
		},
	}
}
