package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"github.com/verdictlayer/nexusrun/internal/credentials"
	"github.com/verdictlayer/nexusrun/internal/login"
)

// defaultRegistry is the hosted NexusRun Cloud registry.
const defaultRegistry = "registry.nexusrun.dev"

func newLoginCmd() *cobra.Command {
	var noBrowser bool
	var name string
	cmd := &cobra.Command{
		Use:   "login [registry]",
		Short: "Sign in to NexusRun Cloud (or another registry that supports it)",
		Long: `Sign in to a registry from the terminal. It prints a short code and a link;
open the link on any device that has a browser, sign in, and approve. This
works on headless machines, over SSH, and in kiosks.

The login is stored in ~/.nexusrun/credentials.json (mode 0600) and used by
'nexus push' and 'nexus pull'. NEXUSRUN_REGISTRY_USER / NEXUSRUN_REGISTRY_PASSWORD,
when set, still take precedence, so CI keeps working unchanged.

Revoke a device any time under Access tokens in the dashboard.

Example:
  nexus login
  nexus push my-agent:1.0.0 registry.nexusrun.dev/acme/my-agent:1.0.0`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := defaultRegistry
			if len(args) == 1 {
				host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(args[0], "https://"), "http://"), "/")
			}
			if name == "" {
				name = clientName()
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			c := &login.Client{Base: baseURL(host)}
			da, err := c.Start(ctx, name)
			if err != nil {
				return err
			}
			link := da.VerificationURIComplete
			if link == "" {
				link = da.VerificationURI
			}
			// The link comes from the registry and is printed and handed to the
			// system opener, so it must be a plain web link and nothing else.
			if !safeLoginLink(link) || !safeLoginLink(da.VerificationURI) {
				return errors.New("the registry returned an unsafe sign-in link; refusing to continue")
			}
			fmt.Printf("\nTo sign in, open this link in a browser:\n\n  %s\n\n", link)
			fmt.Printf("and type this code when asked:  %s\n", da.UserCode)
			fmt.Printf("Only approve it if you started this sign-in yourself.\n\n")
			if !noBrowser && canOpenBrowser() {
				if openBrowser(link) == nil {
					fmt.Println("(opened your browser)")
				}
			}
			fmt.Println("Waiting for approval… (Ctrl+C to cancel)")

			tok, err := c.Poll(ctx, da)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return errors.New("cancelled")
				}
				return err
			}
			if err := credentials.Save(host, credentials.Entry{Token: tok.AccessToken, Org: tok.Org, Scope: tok.Scope, Account: name}); err != nil {
				return fmt.Errorf("signed in, but could not save the login: %w", err)
			}
			access := "pull and push"
			if tok.Scope == "pull" {
				access = "pull only"
			}
			fmt.Printf("\nLogged in to %s as %s (%s).\n", host, orDash(tok.Org), access)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the link instead of opening a browser")
	cmd.Flags().StringVar(&name, "name", "", "name shown when you approve this device (default: user@host)")
	return cmd
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout [registry]",
		Short: "Forget the stored login for a registry",
		Long: `Remove the login saved by 'nexus login'. The access token itself stays
valid until you revoke it under Access tokens in the dashboard.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := hostArg(args)
			ok, err := credentials.Delete(host)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Printf("Not logged in to %s.\n", host)
				return nil
			}
			fmt.Printf("Logged out of %s.\nTo make the token unusable everywhere, revoke it under Access tokens in the dashboard.\n", host)
			return nil
		},
	}
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami [registry]",
		Short: "Show the stored login for a registry",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := hostArg(args)
			if os.Getenv("NEXUSRUN_REGISTRY_PASSWORD") != "" || os.Getenv("REGISTRY_PASSWORD") != "" {
				fmt.Println("Credentials come from the environment (NEXUSRUN_REGISTRY_USER / PASSWORD), which override any stored login.")
			}
			e, ok, err := credentials.Lookup(host)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Printf("Not logged in to %s. Run `%s login`.\n", host, binName)
				return nil
			}
			prefix := e.Token
			if len(prefix) > 16 {
				prefix = prefix[:16] + "…"
			}
			fmt.Printf("%s\n  org:    %s\n  access: %s\n  token:  %s\n  device: %s\n", host, orDash(e.Org), orDash(e.Scope), prefix, orDash(e.Account))
			return nil
		},
	}
}

func hostArg(args []string) string {
	if len(args) == 1 {
		return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(args[0], "https://"), "http://"), "/")
	}
	return defaultRegistry
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// safeLoginLink accepts only https links, or http to a loopback registry, with
// no embedded credentials or control characters.
func safeLoginLink(link string) bool {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	for _, r := range link {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

// baseURL is https except for loopback registries, where plain HTTP is normal.
func baseURL(host string) string {
	h := host
	if u, err := url.Parse("//" + host); err == nil && u.Hostname() != "" {
		h = u.Hostname()
	}
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return "http://" + host
	}
	return "https://" + host
}

// clientName is what the user sees when approving: "user@host".
func clientName() string {
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME")
	}
	host, _ := os.Hostname()
	switch {
	case user != "" && host != "":
		return user + "@" + host
	case host != "":
		return host
	default:
		return "nexus CLI"
	}
}

// canOpenBrowser is false on headless Linux, where xdg-open would just fail
// noisily; the printed link and code are the way in there.
func canOpenBrowser() bool {
	if runtime.GOOS == "linux" {
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	}
	return true
}

func openBrowser(link string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", link)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		c = exec.Command("xdg-open", link)
	}
	return c.Start()
}
