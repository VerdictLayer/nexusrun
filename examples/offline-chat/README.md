# Offline chat smoke-test fixture

Place a small, redistributable GGUF at `model.gguf`, record its SHA-256 in
`nexus.yaml` before release, and follow `docs/RUNTIME_PACKAGES.md`. Model
weights are intentionally not committed; their redistribution terms must be
reviewed separately.

The fixture's targets are declared in `nexus.yaml`. Publish a bundle for a
target after the documented clean-host test passes on that OS and hardware.
