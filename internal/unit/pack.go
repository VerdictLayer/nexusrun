// Package unit builds, stores, and distributes Nexus Units as OCI
// artifacts. Using OCI rather than a bespoke archive format means units
// push and pull from any existing registry — ghcr.io, Docker Hub, ECR,
// Harbor, a local zot — with their auth, signing, replication, and
// retention already solved.
package unit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/lanceseidman/nexusrun/internal/manifest"
	"github.com/lanceseidman/nexusrun/internal/store"
)

// OCI media types for the NexusRun artifact format.
const (
	ArtifactType       = "application/vnd.nexusrun.unit.v1"
	MediaTypeConfig    = "application/vnd.nexusrun.unit.config.v1+json"
	MediaTypeSource    = "application/vnd.nexusrun.unit.source.v1.tar+gzip"
	MediaTypeModelGGUF = "application/vnd.nexusrun.model.v1.gguf"

	// AnnotationModelSource records where a linked (non-sealed) model
	// came from, so a puller can fetch it on first run.
	AnnotationModelSource = "dev.nexusrun.model.source"
	AnnotationModelID     = "dev.nexusrun.model.id"
	AnnotationSealed      = "dev.nexusrun.sealed"
)

// BuildOptions controls packaging.
type BuildOptions struct {
	// Seal embeds model weights as OCI layers, producing a fully
	// self-contained artifact that runs with no network access. When
	// false (the default) models are referenced by source and resolved
	// on first run, keeping artifacts small.
	Seal bool

	// Out, when set, also writes the artifact to a portable .nx file
	// (an OCI image layout tarball) for sneakernet distribution.
	Out string

	Progress func(format string, args ...any)
}

// Built describes the result of a build.
type Built struct {
	Ref        string
	Digest     string
	Size       int64
	Sealed     bool
	LayerCount int
}

// Build packages a unit directory into the local OCI store.
func Build(ctx context.Context, s *store.Store, dir string, opts BuildOptions) (*Built, error) {
	logf := opts.Progress
	if logf == nil {
		logf = func(string, ...any) {}
	}

	m, err := manifest.Load(dir)
	if err != nil {
		return nil, err
	}

	ociStore, err := oci.New(s.UnitsDir())
	if err != nil {
		return nil, fmt.Errorf("open unit store: %w", err)
	}

	var layers []ocispec.Descriptor

	// Layer 1: the unit source tree (everything except ignored paths).
	logf("packing source from %s", dir)
	srcTar, err := tarGzDir(dir)
	if err != nil {
		return nil, err
	}
	srcDesc, err := pushBytes(ctx, ociStore, MediaTypeSource, srcTar, map[string]string{
		ocispec.AnnotationTitle: "source.tar.gz",
	})
	if err != nil {
		return nil, err
	}
	layers = append(layers, srcDesc)
	logf("source layer %s (%s)", shortDigest(srcDesc.Digest.String()), humanSize(srcDesc.Size))

	// Layer 2..n: model weights, embedded only when sealing.
	for _, mod := range m.Models {
		if !opts.Seal {
			logf("model %q linked (source: %s)", mod.ID, mod.Source)
			continue
		}
		logf("resolving model %q for sealing…", mod.ID)
		rm, err := s.Resolve(mod.Source, mod.SHA256, nil)
		if err != nil {
			return nil, fmt.Errorf("seal model %q: %w", mod.ID, err)
		}
		logf("embedding %s (%s) — this copies the weights into the artifact", mod.ID, humanSize(rm.Size))
		f, err := os.Open(rm.Path)
		if err != nil {
			return nil, err
		}
		desc, err := pushReader(ctx, ociStore, MediaTypeModelGGUF, f, rm.Size, map[string]string{
			ocispec.AnnotationTitle: mod.ID + ".gguf",
			AnnotationModelID:       mod.ID,
			AnnotationModelSource:   mod.Source,
		})
		f.Close()
		if err != nil {
			return nil, err
		}
		layers = append(layers, desc)
	}

	// Config: the manifest itself, as canonical JSON.
	cfgJSON, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	cfgDesc, err := pushBytes(ctx, ociStore, MediaTypeConfig, cfgJSON, nil)
	if err != nil {
		return nil, err
	}

	annotations := map[string]string{
		ocispec.AnnotationTitle:       m.Name,
		ocispec.AnnotationVersion:     m.Version,
		ocispec.AnnotationDescription: m.Description,
		ocispec.AnnotationCreated:     time.Now().UTC().Format(time.RFC3339),
		AnnotationSealed:              fmt.Sprintf("%t", opts.Seal),
	}
	if m.Author != "" {
		annotations[ocispec.AnnotationAuthors] = m.Author
	}
	if m.License != "" {
		annotations[ocispec.AnnotationLicenses] = m.License
	}

	root, err := oras.PackManifest(ctx, ociStore, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		Layers:              layers,
		ConfigDescriptor:    &cfgDesc,
		ManifestAnnotations: annotations,
	})
	if err != nil {
		return nil, fmt.Errorf("pack manifest: %w", err)
	}
	if err := ociStore.Tag(ctx, root, m.Ref()); err != nil {
		return nil, fmt.Errorf("tag %s: %w", m.Ref(), err)
	}

	built := &Built{
		Ref:        m.Ref(),
		Digest:     root.Digest.String(),
		Size:       root.Size,
		Sealed:     opts.Seal,
		LayerCount: len(layers),
	}

	if opts.Out != "" {
		if err := ExportFile(ctx, s, m.Ref(), opts.Out); err != nil {
			return nil, err
		}
		logf("exported portable artifact to %s", opts.Out)
	}
	return built, nil
}

// Push copies a unit from the local store to a remote OCI registry.
func Push(ctx context.Context, s *store.Store, ref, target string) (string, error) {
	src, err := oci.New(s.UnitsDir())
	if err != nil {
		return "", err
	}
	repo, err := newRepository(target)
	if err != nil {
		return "", err
	}
	tag := "latest"
	if _, t, ok := splitRefTag(target); ok {
		tag = t
	}
	desc, err := oras.Copy(ctx, src, ref, repo, tag, oras.DefaultCopyOptions)
	if err != nil {
		return "", fmt.Errorf("push %s → %s: %w", ref, target, err)
	}
	return desc.Digest.String(), nil
}

// Pull copies a unit from a remote OCI registry into the local store.
func Pull(ctx context.Context, s *store.Store, target string) (string, error) {
	dst, err := oci.New(s.UnitsDir())
	if err != nil {
		return "", err
	}
	repo, err := newRepository(target)
	if err != nil {
		return "", err
	}
	tag := "latest"
	if _, t, ok := splitRefTag(target); ok {
		tag = t
	}
	local := localRefFor(target, tag)
	if _, err := oras.Copy(ctx, repo, tag, dst, local, oras.DefaultCopyOptions); err != nil {
		return "", fmt.Errorf("pull %s: %w", target, err)
	}
	return local, nil
}

// Resolve returns the manifest and layer descriptors for a locally
// stored unit reference.
func Resolve(ctx context.Context, s *store.Store, ref string) (*manifest.Manifest, ocispec.Manifest, error) {
	var om ocispec.Manifest
	st, err := oci.New(s.UnitsDir())
	if err != nil {
		return nil, om, err
	}
	desc, err := st.Resolve(ctx, ref)
	if err != nil {
		return nil, om, fmt.Errorf("unit %q not found locally: %w", ref, err)
	}
	rc, err := st.Fetch(ctx, desc)
	if err != nil {
		return nil, om, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, om, err
	}
	if err := json.Unmarshal(data, &om); err != nil {
		return nil, om, err
	}

	cfgRC, err := st.Fetch(ctx, om.Config)
	if err != nil {
		return nil, om, err
	}
	defer cfgRC.Close()
	cfgData, err := io.ReadAll(cfgRC)
	if err != nil {
		return nil, om, err
	}
	var m manifest.Manifest
	if err := json.Unmarshal(cfgData, &m); err != nil {
		return nil, om, fmt.Errorf("decode unit config: %w", err)
	}
	return &m, om, nil
}

// Unpack extracts a unit's source layer into destDir and returns the
// paths of any sealed model layers, keyed by model ID.
func Unpack(ctx context.Context, s *store.Store, ref, destDir string) (map[string]string, error) {
	st, err := oci.New(s.UnitsDir())
	if err != nil {
		return nil, err
	}
	_, om, err := Resolve(ctx, s, ref)
	if err != nil {
		return nil, err
	}
	sealed := map[string]string{}
	for _, layer := range om.Layers {
		rc, err := st.Fetch(ctx, layer)
		if err != nil {
			return nil, err
		}
		switch layer.MediaType {
		case MediaTypeSource:
			err = untarGz(rc, destDir)
			rc.Close()
			if err != nil {
				return nil, err
			}
		case MediaTypeModelGGUF:
			// Sealed weights land in the shared blob store, so the same
			// model shared by several units is stored once.
			dest := s.BlobPath(layer.Digest.String())
			if _, statErr := os.Stat(dest); statErr != nil {
				tmp, err := os.CreateTemp(s.BlobsDir(), ".unpack-*")
				if err != nil {
					rc.Close()
					return nil, err
				}
				_, err = io.Copy(tmp, rc)
				tmp.Close()
				if err != nil {
					os.Remove(tmp.Name())
					rc.Close()
					return nil, err
				}
				if err := os.Rename(tmp.Name(), dest); err != nil {
					rc.Close()
					return nil, err
				}
			}
			rc.Close()
			id := layer.Annotations[AnnotationModelID]
			if id == "" {
				id = "main"
			}
			sealed[id] = dest
		default:
			rc.Close()
		}
	}
	return sealed, nil
}

// List returns the references of all locally stored units.
func List(ctx context.Context, s *store.Store) ([]string, error) {
	st, err := oci.New(s.UnitsDir())
	if err != nil {
		return nil, err
	}
	var refs []string
	if err := st.Tags(ctx, "", func(tags []string) error {
		refs = append(refs, tags...)
		return nil
	}); err != nil {
		return nil, err
	}
	return refs, nil
}

// ExportFile writes a unit as a portable OCI image layout tarball.
func ExportFile(ctx context.Context, s *store.Store, ref, out string) error {
	tmpDir, err := os.MkdirTemp("", "nexus-export-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	src, err := oci.New(s.UnitsDir())
	if err != nil {
		return err
	}
	dst, err := oci.New(tmpDir)
	if err != nil {
		return err
	}
	if _, err := oras.Copy(ctx, src, ref, dst, ref, oras.DefaultCopyOptions); err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	return tarDirTo(tmpDir, f)
}

// ImportFile loads a portable .nx artifact into the local store.
func ImportFile(ctx context.Context, s *store.Store, path string) ([]string, error) {
	tmpDir, err := os.MkdirTemp("", "nexus-import-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := untar(f, tmpDir); err != nil {
		return nil, err
	}

	src, err := oci.New(tmpDir)
	if err != nil {
		return nil, err
	}
	dst, err := oci.New(s.UnitsDir())
	if err != nil {
		return nil, err
	}
	var imported []string
	if err := src.Tags(ctx, "", func(tags []string) error {
		imported = append(imported, tags...)
		return nil
	}); err != nil {
		return nil, err
	}
	for _, ref := range imported {
		if _, err := oras.Copy(ctx, src, ref, dst, ref, oras.DefaultCopyOptions); err != nil {
			return nil, err
		}
	}
	return imported, nil
}

// --- helpers --------------------------------------------------------------

func newRepository(target string) (*remote.Repository, error) {
	name, _, _ := splitRefTag(target)
	repo, err := remote.NewRepository(name)
	if err != nil {
		return nil, fmt.Errorf("invalid registry reference %q: %w", target, err)
	}
	// Local and explicitly-insecure registries speak plain HTTP. Every
	// other host must use TLS.
	if isLocalRegistry(repo.Reference.Registry) || os.Getenv("NEXUSRUN_REGISTRY_INSECURE") == "1" {
		repo.PlainHTTP = true
	}
	repo.Client = &auth.Client{
		Client:     retry.DefaultClient,
		Cache:      auth.NewCache(),
		Credential: auth.StaticCredential(repo.Reference.Registry, credentialFor(repo.Reference.Registry)),
	}
	return repo, nil
}

// isLocalRegistry reports whether a registry host is loopback, where
// plain HTTP is normal (local zot, registry:2, CI fixtures).
func isLocalRegistry(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// credentialFor reads registry credentials from the environment. Docker
// config file support is intentionally deferred; env vars work
// identically in CI and on a Raspberry Pi.
func credentialFor(registry string) auth.Credential {
	user := firstEnv("NEXUSRUN_REGISTRY_USER", "REGISTRY_USER")
	pass := firstEnv("NEXUSRUN_REGISTRY_PASSWORD", "REGISTRY_PASSWORD", "GITHUB_TOKEN")
	if user == "" && pass == "" {
		return auth.EmptyCredential
	}
	return auth.Credential{Username: user, Password: pass}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// splitRefTag splits "host/repo:tag" into name and tag, tolerating
// registry hosts that carry a port.
func splitRefTag(ref string) (name, tag string, ok bool) {
	idx := strings.LastIndex(ref, ":")
	if idx < 0 || strings.Contains(ref[idx:], "/") {
		return ref, "", false
	}
	return ref[:idx], ref[idx+1:], true
}

func localRefFor(target, tag string) string {
	name, _, _ := splitRefTag(target)
	base := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		base = name[i+1:]
	}
	return base + ":" + tag
}

func pushBytes(ctx context.Context, p interface {
	Push(context.Context, ocispec.Descriptor, io.Reader) error
	Exists(context.Context, ocispec.Descriptor) (bool, error)
}, mediaType string, data []byte, annotations map[string]string) (ocispec.Descriptor, error) {
	desc := content_NewDescriptorFromBytes(mediaType, data)
	desc.Annotations = annotations
	exists, err := p.Exists(ctx, desc)
	if err != nil {
		return desc, err
	}
	if exists {
		return desc, nil
	}
	return desc, p.Push(ctx, desc, bytes.NewReader(data))
}

func pushReader(ctx context.Context, p interface {
	Push(context.Context, ocispec.Descriptor, io.Reader) error
	Exists(context.Context, ocispec.Descriptor) (bool, error)
}, mediaType string, r io.Reader, size int64, annotations map[string]string) (ocispec.Descriptor, error) {
	// Large weights are streamed through a temp file so the digest can be
	// computed without holding gigabytes in memory.
	tmp, err := os.CreateTemp("", "nexus-layer-*")
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	h := newHasher()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		return ocispec.Descriptor{}, err
	}
	desc := ocispec.Descriptor{
		MediaType:   mediaType,
		Digest:      h.Digest(),
		Size:        size,
		Annotations: annotations,
	}
	exists, err := p.Exists(ctx, desc)
	if err != nil {
		return desc, err
	}
	if exists {
		return desc, nil
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return desc, err
	}
	return desc, p.Push(ctx, desc, tmp)
}

// ignored paths are never packed into the source layer.
func isIgnored(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, p := range parts {
		switch p {
		case ".git", ".venv", "venv", "__pycache__", "node_modules", ".DS_Store", ".nexusrun":
			return true
		}
		if strings.HasSuffix(p, ".nx") {
			return true
		}
	}
	return false
}

func tarGzDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if isIgnored(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		// Zero timestamps keep builds reproducible.
		hdr.ModTime = time.Time{}
		hdr.AccessTime = time.Time{}
		hdr.ChangeTime = time.Time{}
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func untarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	return untar(gz, dest)
}

func untar(r io.Reader, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Reject path traversal before touching the filesystem.
		target := filepath.Join(dest, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("refusing to extract %q outside destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}

func tarDirTo(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
