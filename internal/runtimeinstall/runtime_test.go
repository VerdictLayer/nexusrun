package runtimeinstall

import "testing"

func TestCatalogIsPinnedForSupportedPlatforms(t *testing.T) {
	for _, tt := range []struct{ os, arch string }{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}} {
		r, ok := Recommended(tt.os, tt.arch)
		if !ok {
			t.Fatalf("missing %s/%s", tt.os, tt.arch)
		}
		if r.Version == "" || r.SHA256 == "" {
			t.Errorf("%s/%s is not version and digest pinned: %+v", tt.os, tt.arch, r)
		}
		if len(r.Devices) == 0 || r.Tested == "" {
			t.Errorf("%s/%s lacks tested configuration", tt.os, tt.arch)
		}
	}
	if _, ok := Recommended("plan9", "amd64"); ok {
		t.Error("unsupported platform received a recommendation")
	}
}
