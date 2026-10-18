# Plain-language helper

A small first agent that explains computer terms. The model is downloaded with
Ollama and reused by NexusRun.

Follow the [from-scratch walkthrough](../../docs/QUICKSTART.md) for installation,
the complete `nexus.yaml`, your first answer, and optional offline deployment.

Already have NexusRun and Ollama? From the repository folder:

```sh
ollama pull qwen2.5:0.5b
./bin/nexus run ./examples/plain-helper --backend ollama -p "What is a backup?"
./bin/nexus build ./examples/plain-helper
./bin/nexus run plain-helper:0.1.0 --backend ollama -p "What is a backup?"
```

The `--backend ollama` option uses the running Ollama service, which chooses its
own device. The CPU preference in the YAML applies when using llama.cpp.

This example has no tools, passwords, or private files. Its Linux x86-64 CPU
target also supports the [offline bundle walkthrough](../../docs/OFFLINE_DEMO.md).
