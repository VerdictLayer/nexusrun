package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Building the command tree is not free of failure modes: cobra and pflag
// panic at registration time on a duplicated shorthand, so a flag conflict
// in a rarely used subcommand is a crash on every invocation of the binary,
// including --help. Constructing the whole tree here turns that into a test
// failure instead of a panic in a user's terminal.
func TestCommandTreeBuilds(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("building the command tree panicked: %v", r)
		}
	}()

	root := newRootCmd()
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		// Touching the help text forces flag usage to be rendered, which
		// is where a malformed flag definition surfaces.
		if c.UsageString() == "" {
			t.Errorf("%s has no usage", c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)

	// Spot-check the subcommands this release adds, so a rename does not
	// quietly drop one.
	for _, path := range []string{
		"compose validate", "compose up", "compose down", "compose logs",
		"compose build", "compose push", "compose pull", "compose init", "compose list",
		"bench cache show", "bench cache clear", "bench export",
	} {
		if _, _, err := root.Find(strings.Fields(path)); err != nil {
			t.Errorf("%s is not reachable: %v", path, err)
		}
	}
}

// The compose parent still takes bare unit references, and `up` must not
// be swallowed by that positional form.
func TestComposeSubcommandsBeatPositionalArgs(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"compose", "up"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name() != "up" {
		t.Errorf("`compose up` resolved to %q, not the subcommand", cmd.Name())
	}

	cmd, args, err := root.Find([]string{"compose", "summarizer:0.1.0", "translator:0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name() != "compose" || len(args) != 2 {
		t.Errorf("the positional pipeline form broke: cmd=%q args=%v", cmd.Name(), args)
	}
}

// --auto-model belongs to run, and -f means --follow on compose logs.
func TestFlagsAreWhereTheyBelong(t *testing.T) {
	root := newRootCmd()

	run, _, _ := root.Find([]string{"run"})
	for _, name := range []string{"auto-model", "refresh-bench", "cache-ttl"} {
		if run.Flags().Lookup(name) == nil {
			t.Errorf("run is missing --%s", name)
		}
	}

	logs, _, _ := root.Find([]string{"compose", "logs"})
	if f := logs.Flags().ShorthandLookup("f"); f == nil || f.Name != "follow" {
		t.Errorf("-f on compose logs should be --follow, got %v", f)
	}
	if logs.Flags().Lookup("file") == nil {
		t.Error("compose logs should still accept --file in long form")
	}
}

// `nexus init` is the first command anyone runs. It used to hardcode an
// "ollama:" prefix, so every other source form the scaffold comment
// advertises came out mangled — "ollama:hf:org/repo/file.gguf".
func TestScaffoldModelSource(t *testing.T) {
	tests := []struct{ in, want string }{
		// A bare name is an Ollama model; the tag colon is not a scheme.
		{"llama3.1:8b", "ollama:llama3.1:8b"},
		{"phi3", "ollama:phi3"},
		// Already-qualified sources pass through untouched.
		{"ollama:phi3:latest", "ollama:phi3:latest"},
		{"hf:TheBloke/Llama-2-7B-GGUF/model.gguf", "hf:TheBloke/Llama-2-7B-GGUF/model.gguf"},
		{"https://example.com/model.gguf", "https://example.com/model.gguf"},
		{"http://example.com/model.gguf", "http://example.com/model.gguf"},
		// Paths stay paths.
		{"./local.gguf", "./local.gguf"},
		{"/models/local.gguf", "/models/local.gguf"},
		{"../models/x.gguf", "../models/x.gguf"},
		{`C:\models\x.gguf`, `C:\models\x.gguf`},
		{"weights.gguf", "weights.gguf"},
	}
	for _, tt := range tests {
		if got := scaffoldModelSource(tt.in); got != tt.want {
			t.Errorf("scaffoldModelSource(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
