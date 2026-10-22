package unit

import (
	"testing"

	"github.com/verdictlayer/nexusrun/internal/credentials"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"NEXUSRUN_REGISTRY_USER", "REGISTRY_USER", "NEXUSRUN_REGISTRY_PASSWORD", "REGISTRY_PASSWORD", "GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("NEXUSRUN_HOME", t.TempDir())
}

func TestStoredLoginIsUsedWhenTheEnvironmentIsEmpty(t *testing.T) {
	clearEnv(t)
	if err := credentials.Save("registry.nexusrun.dev", credentials.Entry{Token: "nxr_pat_stored", Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	c := credentialFor("registry.nexusrun.dev")
	if c.Password != "nxr_pat_stored" || c.Username != "acme" {
		t.Fatalf("credential = %+v", c)
	}
}

func TestEnvironmentBeatsAStoredLogin(t *testing.T) {
	clearEnv(t)
	_ = credentials.Save("registry.nexusrun.dev", credentials.Entry{Token: "nxr_pat_stored", Org: "acme"})
	t.Setenv("NEXUSRUN_REGISTRY_USER", "ci")
	t.Setenv("NEXUSRUN_REGISTRY_PASSWORD", "ci-secret")
	c := credentialFor("registry.nexusrun.dev")
	if c.Username != "ci" || c.Password != "ci-secret" {
		t.Fatalf("credential = %+v, want the environment's", c)
	}
}

func TestNoLoginForAnotherRegistry(t *testing.T) {
	clearEnv(t)
	_ = credentials.Save("registry.nexusrun.dev", credentials.Entry{Token: "nxr_pat_stored"})
	if c := credentialFor("ghcr.io"); c.Password != "" || c.Username != "" {
		t.Fatalf("leaked the nexusrun login to ghcr.io: %+v", c)
	}
}

func TestGitHubTokenGoesOnlyToGitHub(t *testing.T) {
	clearEnv(t)
	t.Setenv("GITHUB_TOKEN", "ghs_actions")
	if c := credentialFor("ghcr.io"); c.Password != "ghs_actions" {
		t.Fatalf("ghcr.io credential = %+v, want GITHUB_TOKEN", c)
	}
	if c := credentialFor("registry.example.com"); c.Password != "" {
		t.Fatalf("sent GITHUB_TOKEN to another registry: %+v", c)
	}
}
