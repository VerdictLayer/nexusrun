package store

// ResolveOllama locates the weights of a model Ollama already holds
// ("name:tag", without the "ollama:" prefix) without copying them.
func ResolveOllama(ref string) (*ResolvedModel, error) { return resolveOllama(ref) }

// RecordedDownloads maps each source NexusRun has downloaded (an hf: spec or
// URL) to the digest of the blob it resolved to.
func (s *Store) RecordedDownloads() map[string]string { return s.loadDownloads() }
