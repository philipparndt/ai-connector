# mlxlab's Fish Audio S2 Pro worker: text to speech with mlx-speech (github.com/appautomaton/mlx-speech, MIT), pure
# MLX. Keeps the model loaded. The weights (appautomaton/fishaudio-s2-pro-8bit-mlx: the dual-AR transformer, 8-bit,
# and the codec in codec-mlx/) are under the Fish Audio Research License: non-commercial use only, never passed on.
#
# S2 Pro has no preset voices: a voice is a reference clip with its exact transcript (in the text's language: a German
# text from an English reference keeps the English accent). Without one it invents a voice; the first sentence's
# clip is then the reference of the others, so a longer text keeps one speaker. Inline tags in square brackets
# steer the delivery ([pause], [excited], [whisper], [professional broadcast tone]; no closing tag).
#
# mlx-speech 0.5.6's adapter passes neither seed nor language on to the model: the worker seeds MLX's random state
# itself (one seed per sentence), the language it finds out from the text.
#
# Protocol: one JSON request per line on stdin; events as JSON lines on file descriptor 3 (stdout and stderr stay
# free for the libraries' own output, which mlxlab shows as log lines) — the tts request of speech.py:
#   {"op": "tts", "family": "fish-s2", "dirs": {"model": "/snap"}, "voice": {"mode": "clone"|"default",
#    "ref_audio": "/x.wav", "ref_text": "…"}, "instruct": "calm", "language": "de", "seed": 1,
#    "chunks": [{"text": "…", "pause": 0.3}], "output": "/out.wav"}
#   → progress per sentence; result {"file", "seconds" (generation), "duration" (audio), "peakGB"}
#
# The worker never downloads (HF_HUB_OFFLINE=1): mlxlab fetches the pinned repository and passes its folder.
import ctypes
import gc
import json
import os
import re
import sys
import tempfile
import threading
import time
import traceback

events = os.fdopen(3, "w", buffering=1)
_emit_lock = threading.Lock()


def emit(**kw):
    with _emit_lock:
        events.write(json.dumps(kw) + "\n")
        events.flush()


emit(type="status", text="starting mlx-speech …")
os.environ["HF_HUB_OFFLINE"] = "1"
os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")

import mlx.core as mx  # noqa: E402
import numpy as np  # noqa: E402
import soundfile as sf  # noqa: E402

mx.set_cache_limit(int(float(os.environ.get("MLXLAB_MLX_CACHE_GB", "2")) * 2**30))

# a sentence's audio tokens at most (the codec makes ~21.5 frames a second: ~47 s; a chunk is at most 280 characters)
MAX_NEW_TOKENS = 1024


# ---------------------------------------------------------------- memory: peak physical footprint of this process
class _RUsageV4(ctypes.Structure):
    _fields_ = [("uuid", ctypes.c_uint8 * 16), ("v", ctypes.c_uint64 * 35)]


_libc = ctypes.CDLL("/usr/lib/libSystem.B.dylib")


def footprint():
    ru = _RUsageV4()
    if _libc.proc_pid_rusage(os.getpid(), 4, ctypes.byref(ru)) != 0:
        return 0
    return ru.v[7]  # ri_phys_footprint (Activity Monitor's "Memory"; includes Metal buffers)


class PeakSampler:
    def __init__(self):
        self.peak = footprint()
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()

    def _run(self):
        while not self.stop.wait(0.2):
            self.peak = max(self.peak, footprint())

    def done(self):
        self.stop.set()
        self.thread.join()
        return max(self.peak, footprint()) / 2**30


# ---------------------------------------------------------------- loading
loaded = {"key": None, "model": None}


def model_for(req):
    from mlx_speech.tts import load

    d = (req.get("dirs") or {}).get("model")
    if not d:
        raise ValueError("dirs.model is required")
    if req.get("family") != "fish-s2":
        raise ValueError(f"unknown family {req.get('family')!r} (this runtime speaks fish-s2)")
    if loaded["key"] != d:
        loaded.update(key=None, model=None)
        gc.collect()
        mx.clear_cache()
        emit(type="status", text="loading Fish Audio S2 Pro …")
        t0 = time.time()
        loaded["model"] = load(d)
        loaded["key"] = d
        emit(type="log", text=f"loaded in {time.time() - t0:.1f} s ({footprint() / 2**30:.1f} GB in use)")
    return loaded["model"]


# ---------------------------------------------------------------- text to speech
_TAG = re.compile(r"\[[^\]]*\]")


def said_text(text):
    """The words of a text without its tags (the transcript of a clip made from it)."""
    return " ".join(_TAG.sub(" ", text).split())


def instruct_tag(instruct):
    """An instruction as S2 Pro's inline tag: "calm, slow" → "[calm, slow] "; tags given as they are."""
    instruct = " ".join((instruct or "").split())
    if not instruct:
        return ""
    if instruct.startswith("["):
        return instruct + " "
    return "[" + instruct.strip("[]") + "] "


def tts(req):
    model = model_for(req)
    v = req.get("voice") or {}
    mode = v.get("mode") or "default"
    chunks = req.get("chunks") or []
    if not chunks:
        raise ValueError("no text")
    ref = None
    if mode == "clone":
        if not (v.get("ref_text") or "").strip():
            raise ValueError("Fish S2 Pro clones with the sample's exact transcript: the voice has none")
        emit(type="status", text="encoding the voice's sample …")
        ref = model.prepare_reference(v["ref_audio"], reference_text=v["ref_text"].strip())
    elif mode != "default":
        raise ValueError(f"Fish S2 Pro has no {mode} voices: a cloned voice (a sample and its transcript) or none")
    prefix = instruct_tag(req.get("instruct"))
    seed = int(req.get("seed") or 0)
    parts, sr, t0 = [], 0, time.time()
    tmp = None
    try:
        for i, ch in enumerate(chunks):
            emit(type="status", text=f"sentence {i + 1} of {len(chunks)} …")
            emit(type="progress", step=i, total=len(chunks))
            mx.random.seed((seed + i) % 2**32)
            out = model.generate(prefix + ch["text"], reference_audio=ref, max_new_tokens=MAX_NEW_TOKENS)
            a = np.asarray(out.waveform, dtype=np.float32).reshape(-1)
            if a.size == 0:
                raise RuntimeError(f"no audio for: {ch['text'][:60]}")
            sr = int(out.sample_rate)
            if ref is None and len(chunks) > 1:
                # the invented voice of the first sentence is the voice of the others
                fd, tmp = tempfile.mkstemp(prefix="mlxlab-fish-", suffix=".wav")
                os.close(fd)
                sf.write(tmp, np.clip(a, -1.0, 1.0), sr, subtype="PCM_16")
                ref = model.prepare_reference(tmp, reference_text=said_text(ch["text"]))
            parts.append(a)
            pause = float(ch.get("pause") or 0)
            if pause > 0 and i < len(chunks) - 1:
                parts.append(np.zeros(int(pause * sr), dtype=np.float32))
            mx.clear_cache()
    finally:
        if tmp:
            os.remove(tmp)
    emit(type="progress", step=len(chunks), total=len(chunks))
    wav = np.clip(np.concatenate(parts), -1.0, 1.0)
    seconds = time.time() - t0
    os.makedirs(os.path.dirname(os.path.abspath(req["output"])), exist_ok=True)
    sf.write(req["output"], wav, sr, subtype="PCM_16")
    return seconds, len(wav) / sr


emit(type="ready")
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
        op = req.get("op") or "tts"
        if op != "tts":
            raise ValueError(f"unknown op {op!r} (this runtime only speaks)")
        sampler = PeakSampler()
        try:
            seconds, duration = tts(req)
        finally:
            peak = sampler.done()
        emit(type="result", file=req["output"], seconds=seconds, duration=duration, peakGB=peak)
    except Exception as e:  # noqa: BLE001
        traceback.print_exc()
        emit(type="error", text=f"{type(e).__name__}: {e}")
