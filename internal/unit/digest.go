package unit

import (
	"crypto/sha256"
	"hash"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// content_NewDescriptorFromBytes builds a descriptor for in-memory content.
func content_NewDescriptorFromBytes(mediaType string, data []byte) ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(data),
		Size:      int64(len(data)),
	}
}

// hasher computes a sha256 digest incrementally, so multi-gigabyte model
// layers can be digested while streaming to disk.
type hasher struct{ h hash.Hash }

func newHasher() *hasher { return &hasher{h: sha256.New()} }

func (w *hasher) Write(p []byte) (int, error) { return w.h.Write(p) }

func (w *hasher) Digest() digest.Digest {
	return digest.NewDigestFromBytes(digest.SHA256, w.h.Sum(nil))
}
