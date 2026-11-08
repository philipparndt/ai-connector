# Models as data

A model of a runtime the worker already has is a file, not code: one JSON manifest per model. The worker reads the
manifests when it starts and lists their models in `/api/models` like every other; the server, the connectors and the
UIs see no difference.

| Where | What it is for |
|---|---|
| `internal/engine/catalog/<id>.json` | the catalogue: embedded in the binary, every worker has it after a release |
| `<config dir>/models.d/<id>.json` (`~/.config/mlxlab/models.d/`) | trying a model on one machine, no build: read when the worker starts; only this worker lists the model, so the server sends its jobs only here |

The same file works in both places: try it in `models.d/`, then move it to the catalogue unchanged. A file in
`models.d/` whose id the catalogue has already is left out (the worker says so when it starts); a file that does not
parse or does not pass the checks below is left out with the reason.

Every model of the catalogue is a manifest but the chat models, the embedder and the classifier: their files are
pinned one by one with checksums (`internal/llm`, `internal/models`) and their engine is Go itself.

| `backend` | `kind` | the models | `runtimeArgs` it needs |
|---|---|---|---|
| `mflux` | `image` | FLUX.2 [klein], Z-Image Turbo, Qwen-Image, SeedVR2 | `mflux`: mflux's name for the model |
| `cutout` | `image` | BiRefNet | `side`: the side the model sees |
| `ltx` | `video` | LTX-2.5 | `hq`: the two-stage HQ pipeline of the dev transformer |
| `avatar` | `avatar` | LTX-2.5 avatar, LongCat Avatar | `runtime`: `ltx-a2v` or `longcat` |
| `music` | `music` | ACE-Step, Stable Audio 3, MiniMax Music 3, LeVo 2, HeartMuLa, YuE2 | `runtime` (the song runtime), `workerModel`; `maxSeconds`, `lora` {`file`, `trigger`, `scale`} |
| `speech` | `speech` | Kokoro, Qwen3-TTS, Chatterbox, VoxCPM2, Higgs, Voxtral, Qwen3-ASR, Qwen3-ForcedAligner (mlx-audio); Fish Audio S2 Pro (mlx-speech, the runtime `fish-speech`) | `workerModel` (the family; its tasks, voices, languages and runtime come from it); a family the catalogue does not know: `speech` ("A speech family of its own") |
| `sound` | `sound` | MOSS-SoundEffect | `workerModel` |
| a generic runtime | its `kinds` | a new library's models ("Generic runtimes"); `detect` only here (SAM 3.1) | `workerModel`; `params` |
| a server runtime | `chat` | mlx-lm, mlx-vlm, llama.cpp ("Server runtimes") | `workerModel` (a GGUF file); `images` |
| `comfyui` | `image`, `video` | a ComfyUI workflow ("ComfyUI") | `workflow`, `inputs`, `folders` |

## The manifest

```json
{
  "id": "z-image-turbo",
  "name": "Z-Image Turbo",
  "kind": "image",
  "backend": "mflux",
  "order": 40,
  "about": "Z-Image Turbo, 6B, Apache 2.0: text to image in 9 steps …",
  "licence": "Apache-2.0",
  "size": "5.5 GB",
  "tasks": ["generate"],
  "source": {
    "repo": "filipstrand/Z-Image-Turbo-mflux-4bit",
    "rev": "b3a8f31115a11f2f9e2fa0bfbc8d78dcc3e6568b",
    "files": ["transformer/", "text_encoder/", "vae/", "tokenizer/"]
  },
  "here": ["transformer/*.safetensors", "vae/*.safetensors", "text_encoder/*.safetensors"],
  "defaults": {"width": 1024, "height": 1024, "steps": 9, "quantize": -1},
  "runtimeArgs": {"mflux": "z-image-turbo"}
}
```

| Field | | |
|---|---|---|
| `id` | required | lower case, digits, `.`, `_`, `-`; unique; the name in every API (changing it breaks clients) |
| `name`, `about` | required | as the pickers show them |
| `kind` | required | the backend's (table above) |
| `backend` | required | the runtime that runs it (table above) |
| `order` | | the place in the list among the models of its kind, small first (the pickers' order); the same order: by id. A machine's own models come after the catalogue's of their kind, in their order |
| `licence` | required | the weights' licence as the pickers show it; `nonCommercial: true` when it forbids commercial use |
| `gated` | | Hugging Face wants a token and an accepted licence |
| `size` | required | what a download fetches: bytes (`5905580032`) or `"5.5 GB"`, `"501 MB"` (GB = 2³⁰, MB = 2²⁰) |
| `memory` | | the GB its runtime takes at its peak, where the size misleads (streaming, offloading, quantising on load); the server routes with it (else size × 1.3), plus 6 GB for the rest of the machine |
| `tasks` | | mflux: `generate` (text to image), `edit` (needs reference images), `upscale`; none: generate and edit · cutout: `cutout` · music: `cover`, `repaint`, `extend`, `lego`, `extract`, `restyle` · speech: none, its family says them |
| `source.repo`, `source.rev` | one of `source` / `sources` | one Hugging Face repository and the commit (40 hex digits: pinned, a new revision is a new download) |
| `source.files` | | the file patterns it needs (a folder ends in `/`); none: the whole repository but `source.ignore` |
| `sources` | one of `source` / `sources` | several repositories, each `{key, repo, rev, include, optional}`: the runtime gets each by its key; `optional`: fetched when first used (Qwen3-TTS's Base and VoiceDesign); the model is here when the store has every file of the others |
| `here` | with `source` | the file patterns that must be there for the model to count as downloaded: in the store each with its `.sha256` (verified), in the Hugging Face cache as they are |
| `hereCache` | | other patterns for the Hugging Face cache; `[]`: the runtime does not take the model from there (LTX, the avatars) |
| `declaredSize` | | a downloaded model's size stays `size`, not the folder's (SeedVR2's folder holds both models; BiRefNet's what it loads) |
| `defaults` | | the request fields the UIs start with (`width`, `height`, `steps`, `guidance`, `quantize`, `scale`, …; docs/api.md) |
| `runtimeArgs` | | what the runtime needs to know of the model (table above); mflux also `loader` and `editLoader`: the mflux classes (`"module:Class"`) for text to image and for editing — a model with only one does only that · a generic runtime: `workerModel` |
| `params` | | a generic runtime's model: what it takes besides the request's fields ("Params" below) |

Unknown fields are an error, so a typo does not go unnoticed. A manifest is data — in `models.d` nobody reviews it,
so nothing in it may reach out of its folders or name code: `source.repo` is `owner/name`, file patterns, `here`,
`workerModel` and ComfyUI's `folders` stay inside their folder (no `..`, nothing absolute), a ComfyUI folder is a
plain name (`checkpoints`, `vae`, …), and mflux's `loader`/`editLoader` name a class of `mflux.models` only. Code
comes only through `runtimes.d`, which the worker reads only when allowed.

## Checks

`go test ./internal/engine` loads every manifest of the catalogue: they parse, ids are unique, kinds and backends
exist, revisions are pinned. `TestCatalogGolden` (testdata/catalog.golden.json) pins what users get of every model —
the API, the files and folders, the runtime's arguments, the recommendations; a model moved from Go into a manifest
must leave it as it is.

## Runtimes

A Python runtime is a manifest too: `internal/engine/runtimes/<name>.json` — its worker script (in
`internal/engine/workers/`), an optional build step, and one environment per platform. The packages differ by
platform (PyTorch's CUDA or ROCm wheels, MLX only on Apple silicon), so a worker offers a runtime's models only where
the runtime has an environment for its platform; the others are not listed (and the server routes their jobs to the
workers that have them).

```json
{
  "name": "cutout",
  "script": "birefnet_worker.py",
  "envs": {
    "darwin-arm64": {"python": "3.12", "packages": ["torch==2.14.1", "…"]},
    "linux-cuda":   {"python": "3.12", "packages": ["torch==2.14.1", "…"]},
    "linux-rocm":   {"python": "3.12", "packages": ["torch==2.14.1", "…"], "index": "https://download.pytorch.org/whl/rocm6.4"}
  }
}
```

| Platform | is |
|---|---|
| `darwin-arm64` | a Mac with Apple silicon (MLX, PyTorch on MPS) |
| `linux-cuda`, `linux-rocm`, `linux-cpu` | Linux with an NVIDIA GPU (`nvidia-smi`), with AMD's ROCm (`rocminfo`, `/opt/rocm`), with neither |
| `windows-cuda`, `windows-rocm`, `windows-cpu` | the same on Windows (ROCm: the HIP SDK's `HIP_PATH`) |

`MLXLAB_GPU=cuda|rocm|cpu` overrides what the worker finds. `index` with `indexPackages`: a package index for the
packages named (PyTorch's wheels for a GPU); they are installed from it alone, the others from PyPI alone, so the two
never compete for a name. An environment is installed once, by uv, into
`<config dir>/envs/<name>-<hash>`; the hash is of the Python version, the packages and the index, so a changed
environment is a new directory.

Today: every runtime runs on Apple silicon; BiRefNet also on Linux (CUDA, the CPU, and ROCm — not tried on a Radeon
yet). No Python runtime on Windows yet: the job protocol's events come on file descriptor 3, which Windows does not
pass to a child process. ComfyUI (an HTTP API) and the local servers are not affected.

### Generic runtimes: a new library without Go

A runtime with `"generic": true` needs no Go of its own: its models (`"backend": "<runtime>"`) run in the generic
backend, which speaks the job protocol with the worker script.

| Field | |
|---|---|
| `generic`, `kinds`, `output` | the kinds its models may be, and the extension of what it makes (`png`, `wav`, `mp4`, …); `json`: the file is also the job's `result` (a detector's boxes) |
| `heavy` | its model takes so much memory that the queue unloads the other heavy ones first — the default; `false` for a small one that may stay beside them |

The script reads one JSON request per line on stdin and writes events as JSON lines on file descriptor 3 (`status`,
`progress` with `step`/`total`, `log`, `result` with `file`, `seconds`, `peakGB`, `width`, `height`, or `error` with
`text`; `ready` once it has started). The request is the job's request with the model's `defaults` under it, and:

| Key | |
|---|---|
| `model` | the model's `runtimeArgs.workerModel` (else its id) |
| `path` | the folder of the model's `source` (downloaded and verified) |
| `dirs` | the folders of its `sources`, by key |
| `params` | the model's params, checked and with their defaults |
| `output` | where to write the result |

### Params

A model of a generic runtime says what it takes besides the request's fields:

```json
"params": {"maxSide": {"type": "integer", "default": 4096, "minimum": 256, "maximum": 8192,
                       "description": "the longest side of the result"}}
```

`type` is `integer`, `number`, `string` or `boolean`; `minimum`, `maximum`, `enum`, `default` and `description` as in
JSON Schema. `/api/models` lists them (`params`), so a UI can make its form from them. A job's `params` are checked
when it is submitted — an unknown one, a wrong type or a value out of range is a 400 that names it, before anything is
loaded — and the defaults are filled in.

### A machine's own runtimes

`<config dir>/runtimes.d/<name>/runtime.json` with its script beside it adds a generic runtime to one machine,
without a release. It runs code nobody reviewed, so the worker reads `runtimes.d` only when it is allowed:
`"allowLocalRuntimes": true` in its `worker.json` (or `MLXLAB_ALLOW_LOCAL_RUNTIMES=1` for `ai-connector serve`). A
runtime the catalogue has already is left out.

`docs/examples/` has one that was tried: spandrel with Real-ESRGAN x4 (`runtimes.d/spandrel/`, `models.d/realesrgan-x4.json`)
— copy both into the config dir, allow local runtimes, and `realesrgan-x4` upscales images (512² to 2048² in about
5 s on an M2 Pro, after uv has installed the environment once).

A second one is a detector, the kind `detect`: SAM 3.1 through mlx-vlm (`runtimes.d/sam3`, `models.d/sam3.1.json`).
A job names images and what to find in them (`texts`, e.g. `["ball"]`); the result is JSON with the boxes and scores
per image (docs/api.md "Native jobs API"). About 2 s a picture on an M2 Pro (5.6 GB at the peak); on a handball
video's labelled frames it found the ball as often as SAM 3.1 through its own Swift app (43 of 72 at a score of 0.5).
Uses: a ball or players in sports videos, faces or license plates to blur before sharing, a signature or a table on
a scanned page, counting things on a photo.

### Server runtimes: models the built-in engines do not have

A runtime with `server` is a program that serves an OpenAI-compatible API (`/v1/chat/completions`). The worker
starts it with the model on a free port of 127.0.0.1, waits until `health` answers 200, and asks it like a local
runtime it found running (Ollama, LM Studio). Unlike those it counts as heavy: one such model in memory, the server
stopped when another heavy model comes, when the worker unloads or ends.

```json
{
  "name": "llama-cpp",
  "kinds": ["chat"],
  "server": {"start": ["llama-server", "--model", "{model}", "--host", "127.0.0.1", "--port", "{port}", "--jinja"],
             "health": "/health"},
  "tools": {"darwin-arm64": [{"url": "https://github.com/ggml-org/llama.cpp/releases/download/b11700/llama-b11700-bin-macos-arm64.tar.gz",
                              "sha256": "75d795f8…"}], "…": []}
}
```

Only the worker should ask the server: `server.keyEnv` names the environment variable the program takes an API key
from (`LLAMA_API_KEY`, `MLX_VLM_SERVER_API_KEY`); it gets a random key of this start, which the worker sends with
every request — never as an argument, which every user of the machine can read (a `{key}` in `start` is refused); and after the health check the worker makes sure the port belongs to the process it started
(`lsof`; not on Windows), not to another program that took it in between. mlx-lm's server takes no key: while it
runs, another program of this machine can ask it too (it listens on 127.0.0.1 only).

A server runs in a process group of its own and is ended with it (on Linux it also dies with the worker); its pid is
kept in `<config dir>/run/<runtime>.pid`, so a server a killed worker left behind is ended at the next start. It gets
only what it needs of the worker's environment: the system's variables and those of GPUs and numeric libraries (`serverEnv`), no name with TOKEN, SECRET, PASSWORD or KEY. Where `lsof` is
missing, Linux's `/proc` checks the port; on Windows it is not checked.

`start[0]` is the program: a script of the runtime's Python environment (`envs`, e.g. `mlx_lm.server`), or found in
its `tools` — release archives per platform (`arch`: the processor they are built for, so a machine with another one
is not offered the runtime), fetched once into `<config dir>/tools/<runtime>-<hash>`, checked
against their SHA-256 and unpacked (nothing may point out of that folder). `{model}` is the model's folder, or the
file `runtimeArgs.workerModel` names in it (a GGUF); `{dir}` the folder; `{port}` the port. `server.model`: what a
request names as the model (default: the `{model}` path).

| Runtime | Platforms | Models |
|---|---|---|
| `mlx-lm` | Apple silicon | any chat model mlx-lm knows (Llama, Qwen, Mistral, Gemma, Phi, DeepSeek, …): a folder of MLX weights |
| `mlx-vlm` | Apple silicon | vision language models (Qwen-VL, Gemma 3, SmolVLM, PaddleOCR-VL, …): chat with images (`"images": true` in the model's manifest) |
| `llama-cpp` | Apple silicon (Metal), Linux (CUDA, ROCm, CPU), Windows (CUDA; AMD through Vulkan, RX 6000 too; CPU) | GGUF chat models (`workerModel`: the file) |

Tried here: Qwen3 0.6B through mlx-lm and through llama.cpp (the Mac build, fetched by the worker), SmolVLM 256M
and Qwen3-VL 8B through mlx-vlm with a picture (`docs/examples/models.d`). Qwen3-VL 8B (4-bit, M2 Pro, about 1 s a
picture) read the jersey number on 60 of 68 player crops with a clearly readable number and none wrongly, the
same as mlx-vlm called directly; it takes the model from the Hugging Face cache, as it names no `hereCache`. The Linux and Windows archives are pinned with their
checksums but have not been run here.

## A speech family of its own

mlx-audio knows more text-to-speech models than the catalogue has families for (`speech.py`'s own code for Kokoro,
Qwen3-TTS, Chatterbox, …). A model of another one describes its family in `runtimeArgs.speech`, and `speech.py` runs
it on its general path — mlx-audio's `load_model` with the model type, and `generate` with the keywords the
description allows:

```json
"runtimeArgs": {"workerModel": "kitten",
  "speech": {"modelType": "kitten_tts", "voices": ["Bella", "Jasper", "Luna", "Leo"], "defaultVoice": "Leo",
             "speaks": ["en"], "speed": true}}
```

| Key | |
|---|---|
| `modelType` | mlx-audio's `model_type` (the folder under `mlx_audio/tts/models`) — required |
| `voices`, `defaultVoice` | its preset voices (`voice`), the one taken when a request names none (default: the first) |
| `languages` | the languages it must be told (`lang_code`); `speaks`: those it speaks when not told |
| `clone` | it speaks with a voice of the library made from a sample (`ref_audio`, `ref_text`) |
| `instruct` | it takes an instruction (`instruct`) |
| `speed` | it honours `speed` itself (else the clip is time-stretched afterwards) |

The model's tasks, voices and languages in `/api/models` follow from it, as for the catalogue's families; the aligner
gives word timings as for every speech model. Tried: KittenTTS micro 0.8 (`docs/examples/models.d/kitten-tts-micro.json`,
136 MB) — 10 s of English with the voice Bella in 6.5 s on an M2 Pro; the speech runtime installed by the worker.

### A family in a runtime of its own: Fish Audio S2 Pro

A family whose library is not mlx-audio names its runtime in Go (`speechFamilies`, `runtime`): Fish Audio S2 Pro
(`fish-s2-pro`, workerModel `fish-s2`) runs on mlx-speech 0.5.6 from PyPI in `runtimes/fish-speech.json` (Python
3.13, MLX 0.32.4; the direct dependencies pinned) with its worker `workers/fish_speech.py`, which speaks
`speech.py`'s request. The speech backend keeps one worker per runtime and stops the other before it starts one, so
only one speech model is in memory. The model is a manifest like the others (`catalog/fish-s2-pro.json`:
`appautomaton/fishaudio-s2-pro-8bit-mlx` pinned, 6.3 GB, `memory: 12`), downloaded from Hugging Face on install,
never bundled; the download prepares its environment as for every runtime. Its licence is the Fish Audio Research
License, `nonCommercial: true` (docs/voice-avatar.md).

## ComfyUI

ComfyUI is a server runtime of its own protocol (`server.protocol: "comfyui"`, `runtimes/comfyui.json`): its source
is a pinned release archive (v0.40.0, by SHA-256), its requirements a pinned environment, and the worker starts its
`main.py` (`server.app`) with that environment's python on a free port of 127.0.0.1. A model is a workflow:

```json
"runtimeArgs": {
  "workflow": {"3": {"class_type": "KSampler", "inputs": {"seed": 0, "steps": 20, "cfg": 7, "…": "…"}},
               "4": {"class_type": "CheckpointLoaderSimple", "inputs": {"ckpt_name": "v1-5-pruned-emaonly-fp16.safetensors"}},
               "6": {"class_type": "CLIPTextEncode", "inputs": {"text": "", "clip": ["4", 1]}}, "…": {}},
  "inputs": {"prompt": "6.inputs.text", "seed": "3.inputs.seed", "steps": "3.inputs.steps", "guidance": "3.inputs.cfg",
             "width": "5.inputs.width", "height": "5.inputs.height"},
  "folders": {"checkpoints": ["v1-5-pruned-emaonly-fp16.safetensors"]}
}
```

| Key | |
|---|---|
| `workflow` | the workflow in ComfyUI's API format ("Export (API)" in its menu) |
| `inputs` | where a request's field goes: `node.inputs.name`; the model's `defaults` under the request's fields; a seed the request lacks is drawn |
| `folders` | the model's files (from its `source`, downloaded and verified into the store) by ComfyUI's models folder (`checkpoints`, `diffusion_models`, `text_encoders`, `vae`, `loras`, …): the worker links them there |

ComfyUI starts without custom nodes (`--disable-all-custom-nodes`), with the models' folders of the store as its
model paths (an `extra_model_paths.yaml` in `<config dir>/run/comfyui`; nothing is linked or written into ComfyUI's
own folder, which also works on Windows) and its outputs, inputs and temp files in that run folder; a model whose
folders it does not know yet starts it again. The worker queues the filled workflow (`/prompt`), waits for its
history (`/history`), fetches the first picture or video a node wrote (`/view`), then deletes it and the prompt's
history from ComfyUI; another model than the last frees ComfyUI's memory first (`/free`); a canceled job interrupts
ComfyUI (`/interrupt`). One ComfyUI serves all its models
(it loads what a workflow names); it is heavy and unloads like the others. ComfyUI takes no key: while it runs,
another program of this machine can use it (127.0.0.1 only; the port is checked as for the other servers) — so not
for a machine several people log into. Its folder (with its outputs) is the worker's user's only (0700). Custom
nodes are not installed: a workflow uses ComfyUI's own.

Tried: Stable Diffusion 1.5 (fp16) as `docs/examples/models.d/sd15-comfyui.json` — the worker fetched ComfyUI, built
its environment and fetched the 2.1 GB; 512², 20 steps in 19.7 s on an M2 Pro (MPS), a second job with 12 steps in
8.1 s. Linux (CUDA, CPU; ROCm and Windows CUDA with PyTorch's wheels) has environments, untried; Windows with AMD has
none yet (AMD's Windows wheels are not on a package index).
