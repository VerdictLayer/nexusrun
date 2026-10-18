# Take your helper to an offline computer

An **air-gapped computer** has no connection to outside networks. Prepare the
files on an internet-connected computer, carry them over on a USB drive, and
run the helper there.

Start with the [first-helper walkthrough](QUICKSTART.md). This guide uses the
same `plain-helper` folder and Qwen2.5 0.5B model. All commands in the preparation
section run from the `nexusrun` repository folder.

## What to carry

| Package | What travels with it | What the destination needs |
|---|---|---|
| Sealed `plain-helper.nx` | Instructions and model weights | NexusRun and a compatible llama.cpp runtime already installed |
| Complete `.tar.gz` bundle | Instructions, model weights, NexusRun, llama.cpp, and runtime license files | A compatible Linux computer and a writable folder |

The complete bundle below targets **Linux x86-64 CPU**, using Ubuntu 22.04
and its system libraries. Use a destination with compatible CPU features and
libraries. The runtime built below uses **AVX2, FMA, F16C, BMI2 and SSE4.2**;
`x86_64` alone does not guarantee those features. A CPU-only bundle does not
require a GPU or a GPU driver.

The recipient needs neither Go, Ollama, Docker, nor an API key. The person
preparing the runtime needs build tools; they do this once while online.

## Prepare the bundle while online

### 1. Seal the model

```sh
./bin/nexus build ./plain-helper --seal -o plain-helper.nx
```

This must report `sealed: true`. Keep the export outside the source folder.
The Linux target is already declared in the walkthrough's YAML.

### 2. Build the CPU runtime

On the Ubuntu 22.04 preparation computer, install the additional build tools:

```sh
sudo apt-get install -y build-essential cmake libcurl4-openssl-dev
git clone --depth 1 --branch b5688 https://github.com/ggml-org/llama.cpp ../llama-demo
./scripts/build-managed-runtime.sh ../llama-demo b5688 linux/amd64 'glibc>=2.35' llama-demo.runtime.tar.gz
```

The last command compiles llama.cpp and can take several minutes. It pins the
engine revision, records hashes of the runtime files, and produces
`llama-demo.runtime.tar.gz`. It does not change your system's default engine.

This runtime uses system libraries, including glibc, libstdc++, OpenMP and
libcurl. Prepare and test it on the same OS baseline as the destination;
the package's libc label does not install or check those system libraries.

### 3. Import it and make the complete bundle

```sh
./bin/nexus runtime import ./llama-demo.runtime.tar.gz
./bin/nexus runtime list
```

The output includes an identifier beginning with
`llama.cpp/b5688/linux/amd64/`, followed by a long digest. Copy the **whole
identifier**, not the filesystem path beside it. In the next commands,
replace `PASTE_RUNTIME_ID_HERE` with that identifier:

```sh
./bin/nexus runtime verify 'PASTE_RUNTIME_ID_HERE'
./bin/nexus bundle plain-helper:0.1.0 --target linux/amd64 --runtime 'PASTE_RUNTIME_ID_HERE' -o plain-helper-linux-amd64.tar.gz
sha256sum plain-helper-linux-amd64.tar.gz
```

The bundle command uses the current NexusRun executable. It should report
`Bundled plain-helper:0.1.0 for linux/amd64`. Record the SHA-256 checksum so
the recipient can compare the file after copying it. A matching checksum
detects changed bytes; get the bundle and checksum from someone you trust.

Copy **`plain-helper-linux-amd64.tar.gz`** to the USB drive. This is the
complete deployment archive; you do not also need to copy `.nx` or the
separate runtime archive.

## On the offline computer

Copy the bundle from USB into your home folder. Open Terminal in that folder
and check its checksum:

```sh
sha256sum plain-helper-linux-amd64.tar.gz
```

Compare the result with the checksum recorded on the preparation computer.
Then unpack it into a new folder and run:

```sh
mkdir plain-helper-offline
tar -xzf plain-helper-linux-amd64.tar.gz -C plain-helper-offline
./plain-helper-offline/run -p "What is a backup?"
```

The launcher verifies the runtime files, imports the packaged agent, starts
the bundled llama.cpp server on loopback, and prints a real answer. Look for
**`via llama.cpp/server`** and a nonempty explanation of a backup.

Ask another question to confirm a second run works:

```sh
./plain-helper-offline/run -p "What is a computer folder?"
```

Keep the extracted folder together. It includes the executable, runtime,
agent, and a writable `state` directory created on first run. That directory
contains local model storage and run history. Allow several GB of free space
for the archive, extracted files, and first-run storage.

No outside connection is needed for these commands. The engine uses a local
loopback connection inside the computer; leave loopback enabled if your
organization blocks network access. Bundling does not itself disable the
computer's network interfaces.

## Already have NexusRun and llama.cpp on the destination?

You can transfer just the sealed `.nx` file. On that computer:

```sh
nexus import plain-helper.nx
nexus run plain-helper:0.1.0 --no-daemon --backend llama.cpp/server --device cpu -p "What is a backup?"
```

This command needs a compatible `llama-server` on PATH, an activated managed
runtime, or `NEXUSRUN_LLAMA_SERVER` pointing to that executable. It runs the
sealed weights directly. Use the complete bundle above when you want the
runtime included.

## If the offline run fails

| What you see | What to do |
|---|---|
| Runtime identifier not found during preparation | Copy the full identifier printed by `runtime list`; import the runtime into the same user's NexusRun store. |
| Target is not declared | Use the complete YAML from the first-helper walkthrough and rebuild with `--seal`. |
| `Permission denied` | Move the extracted folder to a writable location that permits running programs; USB drives can prohibit execution. |
| Missing shared library or `GLIBC... not found` | Prepare the runtime on the destination's OS baseline and test there before distribution. |
| `Illegal instruction` | Check the destination CPU against the runtime's CPU features above. |
| Runtime integrity failure | Get a fresh copy from the preparation computer and compare the archive checksum. |

The runtime package schema and deployment acceptance procedure are in
[Managed runtimes and offline bundles](RUNTIME_PACKAGES.md).
