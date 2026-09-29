# Offline chat smoke-test fixture

Place a small, redistributable GGUF at `model.gguf`, record its SHA-256 in
`nexus.yaml` before release, and follow `docs/RUNTIME_PACKAGES.md`. Model
weights are intentionally not committed; their redistribution terms must be
reviewed separately.

This fixture declares Linux amd64 and Linux arm64 CPU targets. Those are build
targets, not automatic compatibility claims: publish a target only after the
documented clean-host test succeeds on the actual OS and hardware baseline.
