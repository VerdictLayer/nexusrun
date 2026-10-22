package fleet

import (
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/unit"
)

func TestFleetRequiresExactlyOneMatchingSealedModel(t *testing.T) {
	m := &manifest.Manifest{Models: []manifest.Model{{ID: "main"}}}
	m.Entrypoint.Type = "chat"
	layer := ocispec.Descriptor{MediaType: unit.MediaTypeModelGGUF, Annotations: map[string]string{unit.AnnotationModelID: "main"}}
	for _, tc := range []struct {
		name   string
		layers []ocispec.Descriptor
		sealed string
		valid  bool
	}{
		{"matching", []ocispec.Descriptor{layer}, "true", true},
		{"missing", nil, "true", false},
		{"duplicate", []ocispec.Descriptor{layer, layer}, "true", false},
		{"wrong-id", []ocispec.Descriptor{{MediaType: unit.MediaTypeModelGGUF, Annotations: map[string]string{unit.AnnotationModelID: "other"}}}, "true", false},
		{"unsealed", []ocispec.Descriptor{layer}, "false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := modelLayer(m, ocispec.Manifest{Layers: tc.layers, Annotations: map[string]string{unit.AnnotationSealed: tc.sealed}})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
