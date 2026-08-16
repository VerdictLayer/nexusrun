package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/secrets"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
)

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Store an agent's credentials outside its unit file",
		Long: `Secrets keeps credentials out of the artifact.

A unit is meant to be committed and pushed to a registry, so a key inside
it is a key in git history. The unit declares only that it needs one; the
value lives here, encrypted, on the machine that runs the agent.

Values are encrypted with a master key at $NEXUSRUN_HOME/master.key,
generated on first use, or taken from NEXUS_MASTER_KEY. Every access is
recorded in $NEXUSRUN_HOME/audit.log by key name — never by value.`,
	}
	cmd.AddCommand(
		newSecretSetCmd(),
		newSecretListCmd(),
		newSecretRotateCmd(),
		newSecretRemoveCmd(),
		newSecretExportCmd(),
		newSecretImportCmd(),
		newSecretCheckCmd(),
		newSecretAuditCmd(),
	)
	return cmd
}

func openSecrets() (*secrets.Store, error) {
	s, err := store.Open()
	if err != nil {
		return nil, err
	}
	return secrets.Open(s)
}

// readSecretValue takes the value from an argument, a file, or stdin.
//
// A value on the command line lands in the shell history of whoever typed
// it, so --file and --stdin exist and are what the help steers toward.
func readSecretValue(args []string, file string, useStdin bool) (string, error) {
	switch {
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return string(data), nil
	case useStdin:
		data, err := readAllStdin()
		if err != nil {
			return "", err
		}
		return strings.TrimRight(data, "\n"), nil
	case len(args) > 0:
		return args[0], nil
	default:
		return "", fmt.Errorf("no value given — pass it as an argument, or use --file or --stdin to keep it out of your shell history")
	}
}

func readAllStdin() (string, error) {
	var b strings.Builder
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	first := true
	for sc.Scan() {
		if !first {
			b.WriteByte('\n')
		}
		b.WriteString(sc.Text())
		first = false
	}
	return b.String(), sc.Err()
}

func newSecretSetCmd() *cobra.Command {
	var device, file, expires string
	var useStdin bool
	cmd := &cobra.Command{
		Use:   "set <agent> <KEY> [value]",
		Short: "Store a secret for an agent",
		Long: `Set stores or replaces a secret.

  nexus secret set my-agent OPENAI_API_KEY sk-xxx
  nexus secret set my-agent OPENAI_API_KEY --stdin < key.txt
  nexus secret set my-agent SSL_CERT --file ./cert.pem
  nexus secret set my-agent API_KEY sk-yyy --device kiosk-01

A device-scoped secret overrides the global one of the same name on that
device, which is how one fleet ships per-site credentials.`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openSecrets()
			if err != nil {
				return err
			}
			value, err := readSecretValue(args[2:], file, useStdin)
			if err != nil {
				return err
			}
			opts := secrets.SetOptions{Device: device}
			if expires != "" {
				d, perr := time.ParseDuration(expires)
				if perr != nil {
					return fmt.Errorf("--expires-in: %w", perr)
				}
				t := time.Now().UTC().Add(d)
				opts.Expires = &t
			}
			if err := st.Set(args[0], args[1], value, opts); err != nil {
				return err
			}
			scope := "all devices"
			if device != "" {
				scope = "device " + device
			}
			fmt.Printf("Stored %s for %s (%s).\n", args[1], args[0], scope)
			return nil
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "scope this secret to one device")
	cmd.Flags().StringVar(&file, "file", "", "read the value from a file")
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "read the value from stdin")
	cmd.Flags().StringVar(&expires, "expires-in", "", "expire after this long (e.g. 720h)")
	return cmd
}

func newSecretListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "list [agent]",
		Aliases: []string{"ls"},
		Short:   "List stored secrets, without their values",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openSecrets()
			if err != nil {
				return err
			}
			agent := ""
			if len(args) == 1 {
				agent = args[0]
			}
			list := st.List(agent)
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(list)
			}
			if len(list) == 0 {
				fmt.Printf("No secrets stored. Add one:\n  %s secret set <agent> <KEY> --stdin\n", binName)
				return nil
			}
			agentW, keyW, scopeW := len("AGENT"), len("KEY"), len("SCOPE")
			for _, s := range list {
				agentW = max(agentW, len(s.Agent))
				keyW = max(keyW, len(s.Key))
				scopeW = max(scopeW, len(s.Scope()))
			}
			row := fmt.Sprintf("  %%-%ds  %%-%ds  %%-%ds  %%7s  %%s\n", agentW, keyW, scopeW)
			fmt.Printf(row, "AGENT", "KEY", "SCOPE", "VERSION", "UPDATED")
			for _, s := range list {
				note := s.UpdatedAt.Local().Format("2006-01-02 15:04")
				if s.Expired() {
					note += "  (expired)"
				} else if s.ExpiresAt != nil {
					note += fmt.Sprintf("  (expires %s)", s.ExpiresAt.Local().Format("2006-01-02"))
				}
				fmt.Printf(row, s.Agent, s.Key, s.Scope(), fmt.Sprint(s.Version), note)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	return cmd
}

func newSecretRotateCmd() *cobra.Command {
	var device, file string
	var useStdin bool
	cmd := &cobra.Command{
		Use:   "rotate <agent> <KEY> [value]",
		Short: "Replace a secret, keeping the old value valid briefly",
		Long: `Rotate replaces a value while the previous one stays accepted for a
grace period (default 5 minutes), so an agent already holding the old
value does not fail mid-request.

During the grace period the previous value is also offered to the agent as
<KEY>_PREVIOUS, so it can accept either.`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openSecrets()
			if err != nil {
				return err
			}
			value, err := readSecretValue(args[2:], file, useStdin)
			if err != nil {
				return err
			}
			if err := st.Rotate(args[0], args[1], value, device); err != nil {
				return err
			}
			fmt.Printf("Rotated %s for %s. The previous value stays valid for %s.\n",
				args[1], args[0], secrets.DefaultGrace)
			return nil
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "rotate the device-scoped secret")
	cmd.Flags().StringVar(&file, "file", "", "read the new value from a file")
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "read the new value from stdin")
	return cmd
}

func newSecretRemoveCmd() *cobra.Command {
	var device string
	cmd := &cobra.Command{
		Use:     "remove <agent> <KEY>",
		Aliases: []string{"rm"},
		Short:   "Delete a secret",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openSecrets()
			if err != nil {
				return err
			}
			removed, err := st.Remove(args[0], args[1], device)
			if err != nil {
				return err
			}
			if !removed {
				return fmt.Errorf("no secret %s for agent %s in that scope", args[1], args[0])
			}
			fmt.Printf("Removed %s for %s.\n", args[1], args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "remove the device-scoped secret")
	return cmd
}

func newSecretExportCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "export [agent]",
		Short: "Write an encrypted backup",
		Long: `Export writes a passphrase-encrypted backup, readable on another
machine that does not have this machine's master key.

The passphrase comes from NEXUS_BACKUP_KEY. Output goes to a file with -o,
or to stdout — it is ciphertext either way, never plaintext.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pass := os.Getenv("NEXUS_BACKUP_KEY")
			if strings.TrimSpace(pass) == "" {
				return fmt.Errorf("set NEXUS_BACKUP_KEY to the passphrase this backup should be encrypted with")
			}
			st, err := openSecrets()
			if err != nil {
				return err
			}
			agent := ""
			if len(args) == 1 {
				agent = args[0]
			}
			data, err := st.Export(agent, pass)
			if err != nil {
				return err
			}
			if out == "" {
				_, werr := os.Stdout.Write(data)
				return werr
			}
			if err := os.WriteFile(out, data, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Wrote %s\n", out)
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "write to this file instead of stdout")
	return cmd
}

func newSecretImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <file>",
		Short: "Restore an encrypted backup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pass := os.Getenv("NEXUS_BACKUP_KEY")
			if strings.TrimSpace(pass) == "" {
				return fmt.Errorf("set NEXUS_BACKUP_KEY to the passphrase this backup was encrypted with")
			}
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			st, err := openSecrets()
			if err != nil {
				return err
			}
			n, err := st.Import(data, pass)
			if err != nil {
				return err
			}
			fmt.Printf("Imported %d secret(s).\n", n)
			return nil
		},
	}
}

func newSecretCheckCmd() *cobra.Command {
	var device string
	cmd := &cobra.Command{
		Use:   "check <ref|dir>",
		Short: "Report whether a unit's declared secrets are present",
		Long: `Check resolves what a unit declares against what is stored, and says
which required secrets are missing — before a run fails halfway through.

It reports presence only. Values are never printed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := store.Open()
			if err != nil {
				return err
			}
			var m *manifest.Manifest
			if info, serr := os.Stat(args[0]); serr == nil && info.IsDir() {
				m, err = manifest.Load(args[0])
			} else {
				m, _, err = unit.Resolve(ctx, s, args[0])
			}
			if err != nil {
				return err
			}
			if len(m.Secrets) == 0 && len(m.Config) == 0 {
				fmt.Printf("%s declares no secrets or config.\n", m.Ref())
				return nil
			}
			st, err := secrets.Open(s)
			if err != nil {
				return err
			}
			keys := make([]string, 0, len(m.Secrets))
			for _, sec := range m.Secrets {
				keys = append(keys, sec.Name)
			}
			resolved, missing, err := st.Env(m.Name, device, keys, 0)
			if err != nil {
				return err
			}
			missingSet := map[string]bool{}
			for _, k := range missing {
				missingSet[k] = true
			}

			fmt.Printf("%s\n\nSecrets:\n", m.Ref())
			var missingRequired []string
			for _, sec := range m.Secrets {
				status := "present"
				switch {
				case missingSet[sec.Name] && sec.Required:
					status = "MISSING (required)"
					missingRequired = append(missingRequired, sec.Name)
				case missingSet[sec.Name]:
					status = "missing (optional)"
				}
				dest := "env " + sec.EnvName()
				if sec.MountPath != "" {
					dest = "file " + sec.MountPath
				}
				fmt.Printf("  %-24s %-20s → %s\n", sec.Name, status, dest)
			}
			if len(m.Config) > 0 {
				fmt.Printf("\nConfig:\n")
				for _, c := range m.Config {
					shown := c.Default
					if shown == "" {
						shown = "(no default)"
					}
					fmt.Printf("  %-24s %-20s → env %s\n", c.Name, shown, c.EnvName())
				}
			}
			_ = resolved
			if len(missingRequired) > 0 {
				return fmt.Errorf("%d required secret(s) missing: %s",
					len(missingRequired), strings.Join(missingRequired, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "check against a device's scoped secrets")
	return cmd
}

func newSecretAuditCmd() *cobra.Command {
	var jsonOut bool
	var limit int
	cmd := &cobra.Command{
		Use:   "audit [agent]",
		Short: "Show the secret access log",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store.Open()
			if err != nil {
				return err
			}
			agent := ""
			if len(args) == 1 {
				agent = args[0]
			}
			entries, err := secrets.Audit(s, agent, limit)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(entries)
			}
			if len(entries) == 0 {
				fmt.Println("No secret access recorded.")
				return nil
			}
			fmt.Printf("%-20s %-8s %-20s %-14s %s\n", "WHEN", "OP", "AGENT", "DEVICE", "KEY")
			for _, e := range entries {
				dev := e.Device
				if dev == "" {
					dev = "—"
				}
				fmt.Printf("%-20s %-8s %-20s %-14s %s\n",
					e.Time.Local().Format("2006-01-02 15:04:05"), e.Op, e.Agent, dev, e.Key)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON")
	cmd.Flags().IntVar(&limit, "limit", 50, "show at most this many entries (0 for all)")
	return cmd
}
