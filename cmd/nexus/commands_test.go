package main

import "testing"

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
