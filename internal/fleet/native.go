package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/verdictlayer/nexusrun/internal/engine"
	"github.com/verdictlayer/nexusrun/internal/eval"
	"github.com/verdictlayer/nexusrun/internal/hardware"
	"github.com/verdictlayer/nexusrun/internal/manifest"
	"github.com/verdictlayer/nexusrun/internal/runtimepkg"
	"github.com/verdictlayer/nexusrun/internal/store"
	"github.com/verdictlayer/nexusrun/internal/unit"
)

func HostHardware() Hardware {
	h := hardware.Detect()
	return Hardware{h.OS, h.Arch, h.CPUCores, h.TotalRAMMB}
}

func pinnedRuntime(home string, r Release) (*engine.LlamaServer, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, errors.New("fleet execution requires Linux amd64")
	}
	path, m, err := runtimepkg.Resolve(home, r.RuntimeID)
	if err != nil {
		return nil, err
	}
	if _, err = runtimepkg.VerifyDirectory(path); err != nil {
		return nil, err
	}
	if m.Accelerator != "cpu" || runtimepkg.HostCompatible(*m) != nil {
		return nil, errors.New("fleet runtime must match the Linux amd64 CPU profile")
	}
	features := strings.Join(hardware.Detect().CPUFeatures, " ")
	for _, f := range m.CPUFeatures {
		if !strings.Contains(" "+features+" ", " "+f+" ") {
			return nil, errors.New("required runtime CPU feature unavailable")
		}
	}
	var libraries []string
	for _, p := range m.LibraryPaths {
		libraries = append(libraries, filepath.Join(path, filepath.FromSlash(p)))
	}
	return engine.NewPinnedServer(filepath.Join(path, filepath.FromSlash(m.Executable)), libraries), nil
}

func modelLayer(m *manifest.Manifest, om ocispec.Manifest) (ocispec.Descriptor, error) {
	if len(m.Models) != 1 || m.Models[0].Auto() || m.Entrypoint.Type != "chat" || len(m.Tools) > 0 || len(m.MCPServers) > 0 || len(m.Secrets) > 0 {
		return ocispec.Descriptor{}, errors.New("fleet release must be a sealed single-model chat unit without tools or secrets")
	}
	var model ocispec.Descriptor
	count := 0
	for _, layer := range om.Layers {
		if layer.MediaType == unit.MediaTypeModelGGUF {
			model = layer
			count++
		}
	}
	if om.Annotations[unit.AnnotationSealed] != "true" || count != 1 || model.Annotations[unit.AnnotationModelID] != m.Models[0].ID {
		return ocispec.Descriptor{}, errors.New("fleet release requires exactly one matching sealed model layer")
	}
	return model, nil
}

func preparedUnit(ctx context.Context, home string, r Release) (*manifest.Manifest, string, string, error) {
	s := &store.Store{Root: ReleaseHome(home, r)}
	if err := unit.Verify(ctx, s, r.Digest); err != nil {
		return nil, "", "", err
	}
	digest, err := unit.Digest(ctx, s, r.Digest)
	if err != nil || digest != r.Digest {
		return nil, "", "", errors.New("local fleet artifact integrity failure")
	}
	if err = unit.ValidateOfflineAgent(ctx, s, r.Digest, "linux/amd64/cpu"); err != nil {
		return nil, "", "", err
	}
	m, om, err := unit.Resolve(ctx, s, r.Digest)
	if err != nil {
		return nil, "", "", err
	}
	if _, err = modelLayer(m, om); err != nil {
		return nil, "", "", err
	}
	dir := filepath.Join(s.Root, "content")
	// Keep source and model paths isolated from every other release.
	sealed, err := unit.Unpack(ctx, s, r.Digest, dir)
	if err != nil {
		return nil, "", "", err
	}
	model := sealed[m.Models[0].ID]
	if model == "" {
		return nil, "", "", errors.New("fleet model must be sealed")
	}
	return m, dir, model, nil
}

func NativePrepare(home string) Prepare {
	return func(ctx context.Context, e Enrollment, r Release) (int, int, string) {
		backend, err := pinnedRuntime(home, r)
		if err != nil {
			return 0, 0, "runtime"
		}
		defer backend.Shutdown()
		if err = os.MkdirAll(ReleaseHome(home, r), 0o700); err != nil {
			return 0, 0, "storage"
		}
		s, err := store.OpenRoot(ReleaseHome(home, r))
		if err != nil {
			return 0, 0, "storage"
		}
		if err = unit.PullPinned(ctx, s, e.Repository, r.Digest, e.Token); err != nil {
			return 0, 0, "artifact"
		}
		m, dir, model, err := preparedUnit(ctx, home, r)
		if err != nil {
			return 0, 0, "artifact"
		}
		suitePath, err := eval.Discover(dir)
		if err != nil {
			return 0, 0, "evaluation"
		}
		suite, err := eval.Load(suitePath)
		if err != nil {
			return 0, 0, "evaluation"
		}
		report, err := eval.Run(ctx, eval.Options{Suite: suite, UnitRef: m.Ref(), UnitDigest: r.Digest, ModelPath: model, ModelRef: m.Models[0].Source, System: m.Entrypoint.SystemPrompt, Context: m.Models[0].Context, Chat: true, Repeats: 1, Devices: []string{"cpu"}, Prefer: []string{"cpu"}, Backends: []engine.Backend{backend}})
		if err != nil || len(report.Targets) != 1 {
			return 0, 0, "evaluation"
		}
		target := report.Targets[0]
		if target.Error != "" || target.Total == 0 || target.Passed != target.Total {
			return target.Passed, target.Total, "evaluation"
		}
		return target.Passed, target.Total, "ok"
	}
}

// Run reads an immutable active snapshot. Updates affect subsequent invocations;
// an invocation already using an older release keeps that snapshot.
func Run(ctx context.Context, home, prompt string, maxTokens int) (*engine.Result, error) {
	unlock, err := Lock(home)
	if err != nil {
		return nil, err
	}
	defer unlock()
	e, err := LoadEnrollment(home)
	if err != nil {
		return nil, err
	}
	s, err := LoadState(home)
	if err != nil {
		return nil, err
	}
	if s.Active == nil {
		return nil, errors.New("no active fleet release; run nexus fleet sync")
	}
	r := s.Active.Release
	c, err := NewClient(e.Repository, e.Token)
	if err != nil {
		return nil, err
	}
	if err = r.Validate(c.Repository, e.Channel); err != nil {
		return nil, err
	}
	backend, err := pinnedRuntime(home, r)
	if err != nil {
		return nil, errors.New("pinned fleet runtime is unavailable or corrupt")
	}
	defer backend.Shutdown()
	local := &store.Store{Root: ReleaseHome(home, r)}
	if err = unit.Verify(ctx, local, r.Digest); err != nil {
		return nil, errors.New("active fleet artifact is unavailable or corrupt")
	}
	m, om, err := unit.Resolve(ctx, local, r.Digest)
	if err != nil {
		return nil, errors.New("active fleet artifact is unavailable or corrupt")
	}
	layer, err := modelLayer(m, om)
	if err != nil {
		return nil, err
	}
	model := local.BlobPath(layer.Digest.String())
	temperature := 0.7
	if m.Models[0].Temperature != nil {
		temperature = *m.Models[0].Temperature
	}
	return backend.Generate(ctx, engine.Request{ModelPath: model, ModelRef: m.Models[0].Source, Prompt: prompt, System: m.Entrypoint.SystemPrompt, Chat: true, Context: m.Models[0].Context, MaxTokens: maxTokens, Temperature: temperature, Device: "cpu"})
}
