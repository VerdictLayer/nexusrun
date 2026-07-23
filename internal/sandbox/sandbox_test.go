package sandbox

import "testing"

// The policy is the security boundary: an undeclared capability must
// never appear in it.
func TestFromCapabilitiesDeniesByDefault(t *testing.T) {
	p := FromCapabilities(nil, "/tmp/unit", "/home/user")
	if p.AllowNetwork {
		t.Error("network allowed with no capabilities declared")
	}
	for _, path := range p.ReadWritePaths {
		if path == "/home/user" {
			t.Error("home directory writable with no storage capability")
		}
	}
	if len(p.ReadWritePaths) != 1 || p.ReadWritePaths[0] != "/tmp/unit" {
		t.Errorf("ReadWritePaths = %v, want only the unit directory", p.ReadWritePaths)
	}
}

func TestCapabilitiesGrantIndependently(t *testing.T) {
	tests := []struct {
		name       string
		caps       []string
		wantNet    bool
		wantHomeRW bool
	}{
		{"none", nil, false, false},
		{"network only", []string{"network"}, true, false},
		{"storage only", []string{"storage"}, false, true},
		{"both", []string{"network", "storage"}, true, true},
		{"unknown capability is ignored", []string{"telepathy"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := FromCapabilities(tt.caps, "/tmp/unit", "/home/user")
			if p.AllowNetwork != tt.wantNet {
				t.Errorf("AllowNetwork = %v, want %v", p.AllowNetwork, tt.wantNet)
			}
			homeRW := false
			for _, path := range p.ReadWritePaths {
				if path == "/home/user" {
					homeRW = true
				}
			}
			if homeRW != tt.wantHomeRW {
				t.Errorf("home writable = %v, want %v", homeRW, tt.wantHomeRW)
			}
		})
	}
}

// The helper runs with HOME rewritten to the unit directory, so an empty
// homeDir must not silently widen the policy.
func TestStorageWithoutHomeGrantsNothingExtra(t *testing.T) {
	p := FromCapabilities([]string{"storage"}, "/tmp/unit", "")
	if len(p.ReadWritePaths) != 1 {
		t.Errorf("ReadWritePaths = %v, want only the unit directory", p.ReadWritePaths)
	}
}
