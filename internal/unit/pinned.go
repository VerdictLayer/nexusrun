package unit

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/verdictlayer/nexusrun/internal/store"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// PullPinned imports exactly one digest using a repository-scoped host credential.
// It does not read or change the user's global registry login or mutable tags.
func PullPinned(ctx context.Context, s *store.Store, repository, wanted, token string) error {
	d, err := digest.Parse(wanted)
	if err != nil || d.Algorithm() != digest.SHA256 || d.Validate() != nil {
		return errors.New("a valid SHA-256 artifact digest is required")
	}
	repo, err := remote.NewRepository(repository)
	if err != nil {
		return errors.New("invalid fleet repository")
	}
	repo.PlainHTTP = isLocalRegistry(repo.Reference.Registry)
	repo.Client = &auth.Client{Client: retry.DefaultClient, Cache: auth.NewCache(), Credential: auth.StaticCredential(repo.Reference.Registry, auth.Credential{Username: "fleet", Password: token})}
	dst, err := oci.New(s.UnitsDir())
	if err != nil {
		return err
	}
	desc, err := oras.Copy(ctx, repo, wanted, dst, wanted, oras.DefaultCopyOptions)
	if err != nil {
		return errors.New("fleet artifact download failed")
	}
	if desc.Digest.String() != wanted {
		return errors.New("fleet artifact digest mismatch")
	}
	return nil
}

// Verify checks the entire local artifact before a fleet candidate is unpacked.
func Verify(ctx context.Context, s *store.Store, ref string) error {
	st, err := oci.New(s.UnitsDir())
	if err != nil {
		return err
	}
	desc, err := st.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	if desc.Size > 1024*1024 {
		return errors.New("fleet manifest too large")
	}
	verify := func(d ocispec.Descriptor) error {
		if d.Digest.Validate() != nil || d.Digest.Algorithm() != digest.SHA256 {
			return errors.New("invalid artifact descriptor")
		}
		r, err := st.Fetch(ctx, d)
		if err != nil {
			return err
		}
		defer r.Close()
		v := d.Digest.Verifier()
		n, err := io.Copy(v, contextReader{ctx, r})
		if err != nil {
			return err
		}
		if n != d.Size || !v.Verified() {
			return errors.New("artifact integrity check failed")
		}
		return nil
	}
	if err = verify(desc); err != nil {
		return err
	}
	r, err := st.Fetch(ctx, desc)
	if err != nil {
		return err
	}
	defer r.Close()
	var m ocispec.Manifest
	if err = json.NewDecoder(io.LimitReader(r, 1024*1024)).Decode(&m); err != nil {
		return err
	}
	if len(m.Layers) > 128 {
		return errors.New("too many fleet artifact layers")
	}
	for _, d := range append([]ocispec.Descriptor{m.Config}, m.Layers...) {
		if err = verify(d); err != nil {
			return err
		}
	}
	return nil
}
