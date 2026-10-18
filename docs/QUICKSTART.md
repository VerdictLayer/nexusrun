# Your first local AI helper

Start with an ordinary question: **“What is a backup?”** By the end, your own
helper will answer it on your computer. You will install a small model, give
it instructions in one file, and package it with NexusRun.

Ollama downloads and runs the model. NexusRun packages the model choice and
your instructions into a reusable **agent**. No account or API key is needed
for this demo.

## Before you start

Use **Ubuntu 22.04 or an Ubuntu 22.04-based Linux x86-64 computer**, with a CPU.
You can check your computer in Settings → About. In a terminal, `uname -m`
should print `x86_64`.

- Have an internet connection for installation and the first model download.
- Allow **8 GB of RAM and 15 GB of free disk space** as a comfortable starting
  budget for this small demo and the build tools. Larger models need more.
  This is a planning budget, not a measured minimum.
- A dedicated graphics card is optional; the offline example runs on CPU.
- Install **[Go 1.26.5 or newer](https://go.dev/doc/install)**. Go builds NexusRun
  from its source files. You only need it on the computer doing the build.

Open **Terminal** from your applications menu. Paste each command block into
it and press Enter. Run the blocks in order, in the same terminal unless a
step says otherwise. Do not type a `$` prompt in front of the commands.

Install the remaining setup tools:

```sh
sudo apt-get update
sudo apt-get install -y git make curl
go version
```

`sudo` may ask for your computer password; the password stays invisible while
you type. `go version` should show version 1.26.5 or newer. If `go` is not found,
finish the Go installation instructions, then reopen Terminal.

Ollama also provides [Windows](https://docs.ollama.com/windows) and
[macOS](https://docs.ollama.com/macos) installers. Its Windows requirement is
Windows 10 22H2 or newer; its Mac requirement is macOS 14 or newer. Use those
official pages for their installation steps and hardware requirements.

## 1. Install Ollama

Go to **[ollama.com/download](https://ollama.com/download)** and choose Linux.
The [official Linux installer](https://docs.ollama.com/linux) uses this command:

```sh
curl -fsSL https://ollama.com/install.sh | sh
```

This downloads and runs Ollama's installation script and may request your
computer password. When it finishes, check that Ollama is available:

```sh
ollama --version
```

If Ollama says it cannot connect to a server, open a **second terminal** and run:

```sh
ollama serve
```

Leave that second terminal open and return to the first one. If it says the
address is already in use, Ollama is already running; continue below.

## 2. Download a small model and try it

We use **[Qwen2.5 0.5B](https://ollama.com/library/qwen2.5:0.5b)**. Its download
is about 400 MB, small enough for a first demo. It is suited to short, simple
questions; check its answers as you would any AI-generated text.

```sh
ollama pull qwen2.5:0.5b
ollama list
ollama run qwen2.5:0.5b "Explain a backup in one short sentence."
```

Wait for the download to finish. `ollama list` should include `qwen2.5:0.5b`,
and the last command should print an answer about keeping a copy of your data.
The wording can vary. You have now run a real model locally.

## 3. Get NexusRun

In the first terminal:

```sh
git clone https://github.com/verdictlayer/nexusrun
cd nexusrun
CGO_ENABLED=0 make build
./bin/nexus --version
./bin/nexus doctor
./bin/nexus models
```

Keep this terminal in the `nexusrun` folder for the rest of the guide.
`./bin/nexus` means “run the NexusRun program in this folder.” You do not
need to change your system's PATH setting.

The version command should print a version identifier. In the doctor report,
look for **`ollama` → `ready`**. The models command should list
`ollama:qwen2.5:0.5b`. Those two checks mean you have both a running engine
and the model this demo uses.

## 4. Create your helper's instructions

```sh
./bin/nexus init plain-helper --model qwen2.5:0.5b
```

Open the new `plain-helper` folder in your file manager. Open `nexus.yaml`
in a plain-text editor, replace its contents with the following, and save it:

```yaml
apiVersion: nexusrun.dev/v1
name: plain-helper
version: 0.1.0
description: Explains everyday computer terms in plain language
license: Apache-2.0

models:
  - id: main
    source: ollama:qwen2.5:0.5b
    context: 2048

entrypoint:
  type: chat
  system_prompt: |
    Explain everyday computer terms in plain English.
    Use two short sentences and one familiar example.

hardware:
  prefer: [cpu]

installation:
  targets:
    - os: linux
      arch: amd64
      device: cpu
```

Keep the spaces at the start of each line; YAML uses them to group settings.
Use spaces rather than Tab. The filename must be `nexus.yaml`, without an
extra `.txt` on the end.

Here is what you just set:

| Setting | What it means |
|---|---|
| `name` and `version` | The label for your packaged helper: `plain-helper:0.1.0`. |
| `source` | Reuse the model Ollama downloaded. No second model download. |
| `context` | How much text the model can consider at once, measured in tokens. |
| `system_prompt` | The instructions that make this helper yours. |
| `hardware` | Prefer CPU when running the model through llama.cpp. Ollama chooses its own device. |
| `installation` | The destination for the optional Linux x86-64 offline bundle. |

A ready-to-run copy is also in
[examples/plain-helper](../examples/plain-helper/README.md).

## 5. Ask your helper a question

```sh
./bin/nexus run ./plain-helper --backend ollama -p "What is a backup?"
```

`-p` introduces your question. `--backend ollama` tells NexusRun to use the
Ollama service you started. Look for **`via ollama`** and an answer about
backups. The device is reported as **`AUTO`** because Ollama chooses it.

Try another question:

```sh
./bin/nexus run ./plain-helper --backend ollama -p "What is a computer folder?"
```

To change the helper's style, edit `system_prompt`, save the file, and run
the command again. Each command is a fresh question; it does not remember
the previous one.

## 6. Package it so you can run it by name

```sh
./bin/nexus build ./plain-helper
./bin/nexus run plain-helper:0.1.0 --backend ollama -p "What is a backup?"
```

The build should end with **`Built plain-helper:0.1.0`** and **`sealed: false`**.
Your helper is now in NexusRun's local store. This package includes your
instructions and model reference; the model stays in Ollama's storage.

If you edit the YAML later, run `build` again before running by name.
Running `./plain-helper` directly always reads the file in that folder.

## Optional: prepare for offline use

After the model is downloaded, you can turn off Wi-Fi or unplug Ethernet
and repeat step 5 on this computer. Keep Ollama running. A successful answer
confirms your installed demo works without an internet connection.

To put the model and instructions into one file:

```sh
./bin/nexus build ./plain-helper --seal -o plain-helper.nx
```

Look for **`sealed: true`**. The `.nx` file is roughly the size of the model,
rather than just a small configuration package. Keep it outside the
`plain-helper` folder so a later build does not package the export itself.

**Sealing includes the model, not the software that runs it.** On a different
offline computer, the `.nx` file needs NexusRun and a compatible llama.cpp
runtime. Importing a sealed model does not install it into Ollama.

For a machine that starts with neither NexusRun nor Ollama, follow
**[Take your helper to an offline computer](OFFLINE_DEMO.md)**. It explains
how to prepare one complete Linux bundle while online, copy it by USB, and
run it with a single command on the destination.

## If something goes wrong

| What you see | What to do |
|---|---|
| `go`, `git`, `make`, or `ollama`: command not found | Finish that tool's installation above, then reopen Terminal. |
| `./bin/nexus`: no such file | Return to the `nexusrun` folder and repeat the build in step 3. |
| `plain-helper/nexus.yaml already exists` | Your helper is already created. Continue by editing that file in step 4. |
| Cannot connect to Ollama | Start `ollama serve` in a second terminal, or open the Ollama application. |
| `ollama model ... not found` | Repeat `ollama pull qwen2.5:0.5b`. Run Ollama and NexusRun as the same user, on the same computer. If you set `OLLAMA_MODELS`, use that setting in both. |
| YAML error or unknown field | Compare the file with step 4, including indentation and spelling. |
| First answer takes a while | The model must load into memory. Wait for it to finish; later Ollama calls can reuse the loaded model. |
| Out-of-memory error | Close other applications and retry this small model. Memory use also depends on context length and other loaded models. |

For all command options, see the [reference](REFERENCE.md).
