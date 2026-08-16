package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"

	"github.com/verdictlayer/nexusrun/internal/store"
)

// OCI media types for a packaged workflow. A workflow is its own artifact
// type rather than a unit with extra fields: `nexus run` must refuse a
// workflow ref with a clear message instead of half-executing it, and a
// registry listing should say which of the two a tag holds.
const (
	ArtifactType     = "application/vnd.nexusrun.workflow.v1"
	MediaTypeConfig  = "application/vnd.nexusrun.workflow.config.v1+json"
	MediaTypeCompose = "application/vnd.nexusrun.workflow.compose.v1+yaml"

	// AnnotationAgents records the agent count so `nexus list` can describe
	// a pulled workflow without fetching its config blob.
	AnnotationAgents = "dev.nexusrun.workflow.agents"
)

// Load reads a workflow from a file path or a directory containing one.
func Load(path string) (*Spec, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		path = filepath.Join(path, FileName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Built describes a packaged workflow.
type Built struct {
	Ref    string
	Digest string
	Size   int64
	Agents int
}

// Build packages a workflow file into the local OCI store.
//
// Only the workflow file travels. The agents it names are unit references,
// resolved at run time from the same registry — embedding them would
// duplicate artifacts that are already content-addressed and shared, and
// would freeze a workflow to unit revisions its author never pinned.
func Build(ctx context.Context, s *store.Store, path string) (*Built, error) {
	spec, err := Load(path)
	if err != nil {
		return nil, err
	}
	src, err := readSpecFile(path)
	if err != nil {
		return nil, err
	}

	ociStore, err := oci.New(s.UnitsDir())
	if err != nil {
		return nil, fmt.Errorf("open unit store: %w", err)
	}

	composeDesc, err := pushBytes(ctx, ociStore, MediaTypeCompose, src, map[string]string{
		ocispec.AnnotationTitle: FileName,
	})
	if err != nil {
		return nil, err
	}

	cfgJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	cfgDesc, err := pushBytes(ctx, ociStore, MediaTypeConfig, cfgJSON, nil)
	if err != nil {
		return nil, err
	}

	annotations := map[string]string{
		ocispec.AnnotationTitle:       spec.Name,
		ocispec.AnnotationVersion:     spec.Version,
		ocispec.AnnotationDescription: spec.Description,
		ocispec.AnnotationCreated:     time.Now().UTC().Format(time.RFC3339),
		AnnotationAgents:              fmt.Sprint(len(spec.Agents)),
	}
	if spec.Author != "" {
		annotations[ocispec.AnnotationAuthors] = spec.Author
	}
	if spec.License != "" {
		annotations[ocispec.AnnotationLicenses] = spec.License
	}

	root, err := oras.PackManifest(ctx, ociStore, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		Layers:              []ocispec.Descriptor{composeDesc},
		ConfigDescriptor:    &cfgDesc,
		ManifestAnnotations: annotations,
	})
	if err != nil {
		return nil, fmt.Errorf("pack manifest: %w", err)
	}
	if err := ociStore.Tag(ctx, root, spec.Ref()); err != nil {
		return nil, fmt.Errorf("tag %s: %w", spec.Ref(), err)
	}

	return &Built{
		Ref:    spec.Ref(),
		Digest: root.Digest.String(),
		Size:   root.Size,
		Agents: len(spec.Agents),
	}, nil
}

// Resolve loads a workflow from a locally stored reference.
func Resolve(ctx context.Context, s *store.Store, ref string) (*Spec, error) {
	st, err := oci.New(s.UnitsDir())
	if err != nil {
		return nil, err
	}
	desc, err := st.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("workflow %q not found locally: %w", ref, err)
	}
	data, err := fetchAll(ctx, st, desc)
	if err != nil {
		return nil, err
	}
	var om ocispec.Manifest
	if err := json.Unmarshal(data, &om); err != nil {
		return nil, err
	}
	if om.ArtifactType != ArtifactType {
		return nil, fmt.Errorf("%s is not a workflow (artifact type %s) — `nexus run` handles units, `nexus compose up` handles workflows",
			ref, om.ArtifactType)
	}
	cfg, err := fetchAll(ctx, st, om.Config)
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(cfg, &spec); err != nil {
		return nil, fmt.Errorf("decode workflow config: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	spec.applyDefaults()
	return &spec, nil
}

// readSpecFile returns the workflow file's bytes, resolving a directory to
// the canonical file name inside it.
func readSpecFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		path = filepath.Join(path, FileName)
	}
	return os.ReadFile(path)
}

func fetchAll(ctx context.Context, st *oci.Store, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := st.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func pushBytes(ctx context.Context, p interface {
	Push(context.Context, ocispec.Descriptor, io.Reader) error
	Exists(context.Context, ocispec.Descriptor) (bool, error)
}, mediaType string, data []byte, annotations map[string]string) (ocispec.Descriptor, error) {
	desc := ocispec.Descriptor{
		MediaType:   mediaType,
		Digest:      digest.FromBytes(data),
		Size:        int64(len(data)),
		Annotations: annotations,
	}
	exists, err := p.Exists(ctx, desc)
	if err != nil {
		return desc, err
	}
	if exists {
		return desc, nil
	}
	return desc, p.Push(ctx, desc, bytes.NewReader(data))
}
