package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/pyenv"
)

func byID(ms []*Model) map[string]*Model {
	out := map[string]*Model{}
	for _, m := range ms {
		out[m.ID] = m
	}
	return out
}

// Kokoro 82M in the catalogue: pinned, Apache-2.0, its voices (without Japanese) with names and languages.
func TestKokoroCatalog(t *testing.T) {
	m := byID(speechModels())["kokoro-82m"]
	if m == nil || m.Backend != "speech" || m.Kind != Speech || m.Licence != "Apache-2.0" || m.NonCommercial || m.workerModel != "kokoro" {
		t.Fatalf("kokoro %+v", m)
	}
	hr := m.repos[0]
	if hr.repo != "mlx-community/Kokoro-82M-bf16" || len(hr.rev) != 40 || m.Size < 300<<20 || m.Size > 400<<20 {
		t.Fatalf("repo %+v size %d", hr, m.Size)
	}
	keep := hr.keep()
	for p, want := range map[string]bool{"config.json": true, "kokoro-v1_0.safetensors": true, "voices/af_heart.safetensors": true,
		"voices/af_heart.pt": false, "samples/af_heart_0.wav": false, "README.md": false} {
		if keep(p) != want {
			t.Errorf("keep(%q) = %v", p, !want)
		}
	}
	a := m.Audio
	if a == nil || a.Instruct || !a.Words || a.MaxChars != 5000 ||
		!reflect.DeepEqual(a.Languages, []string{"en", "es", "fr", "it", "pt", "hi", "zh"}) || len(a.Voices) != 49 {
		t.Fatalf("caps %+v", a)
	}
	if a.Voices[0] != (PresetVoice{"af_heart", "Heart", "en"}) {
		t.Errorf("first voice %+v", a.Voices[0])
	}
	langs := map[string]bool{}
	for _, v := range a.Voices {
		if v.Language == "" || strings.HasPrefix(v.ID, "j") {
			t.Errorf("voice %+v", v)
		}
		langs[v.Language] = true
	}
	if len(langs) != 7 {
		t.Errorf("languages of the voices %v", langs)
	}
	for id, want := range map[string]PresetVoice{
		"bf_emma":    {"bf_emma", "Emma (British)", "en"},
		"ff_siwis":   {"ff_siwis", "Siwis", "fr"},
		"zm_yunjian": {"zm_yunjian", "Yunjian", "zh"},
		"pf_dora":    {"pf_dora", "Dora", "pt"},
	} {
		if i := slices.IndexFunc(a.Voices, func(v PresetVoice) bool { return v.ID == id }); i < 0 || a.Voices[i] != want {
			t.Errorf("%s: %+v", id, a.Voices)
		}
	}
	// a language's default voice
	f := speechFamilies["kokoro"]
	for lang, want := range map[string]string{"en": "af_heart", "fr": "ff_siwis", "zh": "zf_xiaobei", "de": "af_heart", "": "af_heart"} {
		if got := f.preset(lang); got != want {
			t.Errorf("preset(%q) = %q, want %q", lang, got, want)
		}
	}
}

// The other models' capabilities as vboard's generators show them.
func TestAudioCaps(t *testing.T) {
	sp := byID(speechModels())
	if sp["qwen3-asr"].Audio != nil || sp["qwen3-forced-aligner"].Audio != nil {
		t.Error("the transcriber and the aligner are no generators")
	}
	q := sp["qwen3-tts-1.7b"].Audio
	if !q.Instruct || q.Words || len(q.Voices) != 9 || q.Voices[5] != (PresetVoice{"Ryan", "Ryan", "en"}) || len(q.Languages) != 10 {
		t.Errorf("qwen %+v", q)
	}
	if v := sp["voxtral-tts"].Audio; v.Instruct || v.Voices[9] != (PresetVoice{"de_male", "DE male", "de"}) || v.Voices[0].Language != "en" ||
		len(v.Languages) != 9 {
		t.Errorf("voxtral %+v", v)
	}
	if v := sp["voxcpm2"].Audio; len(v.Voices) != 0 || !v.Instruct || v.Languages != nil {
		t.Errorf("voxcpm2 %+v", v)
	}
	if c := sp["chatterbox-v3"].Audio; len(c.Languages) != 23 || c.Voices[0] != (PresetVoice{"default", "Default", "en"}) {
		t.Errorf("chatterbox %+v", c)
	}
	mu := byID(musicModels())
	for id, want := range map[string]AudioCaps{
		"ace-step-1.5-turbo":    {MaxSeconds: 600, Lyrics: true, Instrumental: true, BPM: true},
		"stable-audio-3-small":  {MaxSeconds: 120, Instrumental: true, BPM: true},
		"stable-audio-3-medium": {MaxSeconds: 380, Instrumental: true, BPM: true},
		"minimax-music3-8bit":   {MaxSeconds: 300, Lyrics: true, Instrumental: true, BPM: true},
		"levo2-large":           {MaxSeconds: 270, Lyrics: true, Instrumental: true},
		"heartmula-3b":          {MaxSeconds: 240, Lyrics: true, Instrumental: true},
		"yue2-3b":               {MaxSeconds: 240, Lyrics: true, Instrumental: true},
	} {
		if m := mu[id]; m == nil || !reflect.DeepEqual(*m.Audio, want) {
			t.Errorf("%s: %+v", id, m)
		}
	}
	if mu["ace-step-1.5-turbo"].Defaults.Duration != 60 {
		t.Error("ACE-Step's default length")
	}
	s := soundModels()[0]
	if s.Audio == nil || s.Audio.MaxSeconds != 30 || s.Defaults.Duration != 8 {
		t.Errorf("sound %+v", s.Audio)
	}
	// the catalogue's JSON carries them (the hub reads "audio")
	b, _ := json.Marshal(sp["kokoro-82m"])
	if !strings.Contains(string(b), `"audio":{"voices":[{"id":"af_heart","name":"Heart","language":"en"}`) {
		t.Errorf("json %s", b[:min(len(b), 400)])
	}
}

// fakeSpeechWorker speaks the worker protocol: it writes the request into the output file and answers with the
// words of an English Kokoro request or of a request with the aligner.
const fakeSpeechWorker = `
import json, os, sys
ev = os.fdopen(3, "w", buffering=1)
def emit(**kw): ev.write(json.dumps(kw) + "\n")
emit(type="ready")
for line in sys.stdin:
    req = json.loads(line)
    with open(req["output"], "w") as f:
        json.dump(req, f)
    res = dict(type="result", file=req["output"], seconds=0.2, duration=1.5, peakGB=1.0)
    if req["family"] == "kokoro" and req["language"] == "en" or req["dirs"].get("align"):
        res["words"] = [{"text": "Hello", "start": 0.1, "end": 0.4}, {"text": "board", "start": 0.5, "end": 0.9}]
    emit(**res)
`

func fakeSpeechBackend(t *testing.T) *SpeechBackend {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	root := t.TempDir()
	env := pyenv.Env{Name: "fake-speech", Python: "x"}
	dir := env.Dir(root)
	must(t, os.MkdirAll(filepath.Join(dir, "bin"), 0o755))
	fake := filepath.Join(root, "fake.py")
	must(t, os.WriteFile(fake, []byte(fakeSpeechWorker), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "bin", "python"), []byte("#!/bin/sh\nexec "+py+" -u "+fake+"\n"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, ".mlxlab-ready"), nil, 0o644))
	b := &SpeechBackend{Root: root, env: &env}
	b.fetch = func(_ context.Context, _ string, hr hubRepo, _ Emit) (string, error) { return "/w/" + hr.key, nil }
	return b
}

// A speech job's request to the worker and its result: Kokoro's voice sets the language, the default voice follows
// the language, speed is Kokoro's own, words pass through; a language it does not speak is refused.
func TestSpeechRequestKokoro(t *testing.T) {
	b := fakeSpeechBackend(t)
	defer b.Unload()
	m := byID(speechModels())["kokoro-82m"]
	run := func(r Request) (map[string]any, map[string]any, error) {
		t.Helper()
		var file string
		var res map[string]any
		err := b.Run(context.Background(), m, r, t.TempDir(), func(ev Event) {
			switch ev.Type {
			case "file":
				file = ev.File
			case "result":
				res = ev.Data
			}
		})
		if err != nil {
			return nil, nil, err
		}
		raw, err := os.ReadFile(file)
		must(t, err)
		var req map[string]any
		must(t, json.Unmarshal(raw, &req))
		return req, res, nil
	}
	req, res, err := run(Request{Text: "Bonjour tout le monde, ceci est un test.", Voice: "ff_siwis", Speed: 1.2})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := req["voice"].(map[string]any)
	if req["family"] != "kokoro" || req["language"] != "fr" || v["mode"] != "preset" || v["preset"] != "ff_siwis" ||
		req["speed"] != 1.2 || req["dirs"].(map[string]any)["model"] != "/w/model" {
		t.Fatalf("request %v", req)
	}
	if res["language"] != "fr" || res["text"] != "Bonjour tout le monde, ceci est un test." || res["words"] != nil ||
		res["voice"] != (PresetVoice{"ff_siwis", "Siwis", "fr"}) {
		t.Fatalf("result %v", res)
	}
	// the language's default voice; English: the words
	req, _, _ = run(Request{Text: "Hola a todos, esto es una prueba.", Language: "es"})
	if v := req["voice"].(map[string]any); v["preset"] != "ef_dora" || req["language"] != "es" {
		t.Fatalf("spanish %v", req)
	}
	_, res, _ = run(Request{Text: "Hello board, how are you today?"})
	if w, _ := res["words"].([]Word); len(w) != 2 || w[1] != (Word{"board", 0.5, 0.9}) || res["voice"] != (PresetVoice{"af_heart", "Heart", "en"}) {
		t.Fatalf("english %v", res)
	}
	// not German
	if _, _, err := run(Request{Text: "Guten Morgen", Language: "de"}); err == nil || !strings.Contains(err.Error(), "does not speak") {
		t.Fatalf("german %v", err)
	}
	// a voice of another model
	if _, _, err := run(Request{Text: "Hello", Voice: "Ryan"}); err == nil {
		t.Fatal("Ryan is no Kokoro voice")
	}
}

// Another family's result: the preset's name, no words.
func TestSpeechRequestQwen(t *testing.T) {
	b := fakeSpeechBackend(t)
	defer b.Unload()
	m := byID(speechModels())["qwen3-tts-1.7b"]
	var res map[string]any
	err := b.Run(context.Background(), m, Request{Text: "Guten Morgen, wie geht es dir heute?", Instruct: "warm"}, t.TempDir(), func(ev Event) {
		if ev.Type == "result" {
			res = ev.Data
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res["language"] != "de" || res["voice"] != (PresetVoice{"Ryan", "Ryan", "en"}) || res["words"] != nil {
		t.Fatalf("result %v", res)
	}
}

// A clip gets the aligner's folder when the aligner is on disk and times the language — never fetched for a clip;
// the words come back for any model then.
func TestSpeechRequestAligner(t *testing.T) {
	b := fakeSpeechBackend(t)
	defer b.Unload()
	here := true
	b.hasAligner = func() bool { return here }
	var fetched []string
	b.fetch = func(_ context.Context, _ string, hr hubRepo, _ Emit) (string, error) {
		fetched = append(fetched, hr.key)
		return "/w/" + hr.key, nil
	}
	sp := byID(speechModels())
	run := func(model string, r Request) (map[string]any, map[string]any) {
		t.Helper()
		var file string
		var res map[string]any
		err := b.Run(context.Background(), sp[model], r, t.TempDir(), func(ev Event) {
			switch ev.Type {
			case "file":
				file = ev.File
			case "result":
				res = ev.Data
			}
		})
		must(t, err)
		raw, err := os.ReadFile(file)
		must(t, err)
		var req map[string]any
		must(t, json.Unmarshal(raw, &req))
		return req["dirs"].(map[string]any), res
	}
	dirs, res := run("qwen3-tts-1.7b", Request{Text: "Guten Morgen, wie geht es dir heute?", Language: "de"})
	if w, _ := res["words"].([]Word); dirs["align"] != "/w/align" || dirs["model"] != "/w/custom" || len(w) != 2 {
		t.Fatalf("german with the aligner: %v %v", dirs, res)
	}
	// a language it does not time (Hindi, Japanese): none, and it is not fetched
	fetched = nil
	if dirs, _ := run("kokoro-82m", Request{Text: "Namaste, yah ek chhota sa parikshan hai.", Voice: "hf_alpha"}); dirs["align"] != nil ||
		slices.Contains(fetched, "align") {
		t.Fatalf("hindi: %v %v", dirs, fetched)
	}
	if dirs, _ := run("qwen3-tts-1.7b", Request{Text: "Konnichiwa, kore wa tesuto desu.", Language: "ja"}); dirs["align"] != nil {
		t.Fatalf("japanese: %v", dirs)
	}
	// not on disk: none, and no words but Kokoro's English (its tokens)
	here = false
	fetched = nil
	dirs, res = run("qwen3-tts-1.7b", Request{Text: "Guten Morgen, wie geht es dir heute?", Language: "de"})
	if dirs["align"] != nil || slices.Contains(fetched, "align") || res["words"] != nil {
		t.Fatalf("german without the aligner: %v %v %v", dirs, fetched, res)
	}
	if dirs, res := run("kokoro-82m", Request{Text: "Hello board, how are you today?"}); dirs["align"] != nil || res["words"] == nil {
		t.Fatalf("english kokoro without the aligner: %v %v", dirs, res)
	}
	// the aligner says nothing itself
	if err := b.Run(context.Background(), sp["qwen3-forced-aligner"], Request{Text: "Hello"}, t.TempDir(), func(Event) {}); err == nil ||
		!strings.Contains(err.Error(), "does not speak") {
		t.Fatalf("aligner job: %v", err)
	}
}

// A clip stretched to another speed (a model without its own) moves its words with it.
func TestStretchWords(t *testing.T) {
	got := stretchWords([]Word{{"a", 0.5, 1.0}, {"b", 1.2, 2.4}}, 1.25)
	if !reflect.DeepEqual(got, []Word{{"a", 0.4, 0.8}, {"b", 0.96, 1.92}}) {
		t.Fatalf("%v", got)
	}
	if got := stretchWords([]Word{{"a", 1, 2}}, 4); got[0] != (Word{"a", 0.5, 1}) { // at most ×2, as stretch
		t.Fatalf("%v", got)
	}
}

// Fish S2 Pro runs in a worker of its own runtime (fish-speech): its request is speech.py's (a cloned voice with the
// sample and its transcript, the instruction, the seed), never with the aligner (that is mlx-audio's); the speech
// runtime's worker is stopped before it starts, so one of them keeps a model in memory.
func TestSpeechRequestFish(t *testing.T) {
	b := fakeSpeechBackend(t)
	defer b.Unload()
	b.hasAligner = func() bool { return true }
	sample := filepath.Join(t.TempDir(), "de-male.wav")
	must(t, os.WriteFile(sample, []byte("RIFF"), 0o644))
	b.Voices = fakeVoices{"de-male": {ID: "de-male", Name: "de-male", Kind: "clone", Sample: sample,
		Transcript: "In diesem Tutorial zeichnen wir eine Skizze.", Language: "de"}}
	sp := byID(speechModels())
	run := func(model string, r Request) (map[string]any, map[string]any) {
		t.Helper()
		var file string
		var res map[string]any
		must(t, b.Run(context.Background(), sp[model], r, t.TempDir(), func(ev Event) {
			switch ev.Type {
			case "file":
				file = ev.File
			case "result":
				res = ev.Data
			}
		}))
		raw, err := os.ReadFile(file)
		must(t, err)
		var req map[string]any
		must(t, json.Unmarshal(raw, &req))
		return req, res
	}
	run("qwen3-tts-1.7b", Request{Text: "Guten Morgen, wie geht es dir heute?", Language: "de"})
	speech := b.ws["speech"]
	if speech == nil || !speech.running() {
		t.Fatal("the speech runtime's worker is not running")
	}
	req, res := run("fish-s2-pro", Request{Text: "Öffne den Reiter Parameter gleich neben den Features.", Voice: "de-male",
		Instruct: "calm", Seed: 3})
	v := req["voice"].(map[string]any)
	dirs := req["dirs"].(map[string]any)
	if req["family"] != "fish-s2" || v["mode"] != "clone" || v["ref_audio"] != sample || v["ref_text"] != "In diesem Tutorial zeichnen wir eine Skizze." ||
		req["language"] != "de" || req["instruct"] != "calm" || req["seed"] != 3.0 || dirs["model"] != "/w/model" || dirs["align"] != nil {
		t.Fatalf("request %v", req)
	}
	if res["voice"] != (PresetVoice{"de-male", "de-male", "de"}) || res["words"] != nil {
		t.Fatalf("result %v", res)
	}
	if speech.running() || b.ws["fish-speech"] == nil || !b.ws["fish-speech"].running() {
		t.Fatal("the speech runtime's worker still runs beside Fish S2 Pro's")
	}
	if _, err := os.Stat(filepath.Join(b.env.Dir(b.Root), "mlxlab_fish_speech.py")); err != nil {
		t.Fatalf("Fish S2 Pro's script: %v", err)
	}
	if runtimeName(sp["fish-s2-pro"]) != "fish-speech" || runtimeName(sp["voxcpm2"]) != "speech" {
		t.Fatal("runtime names")
	}
	// no presets, no designed voices
	if err := b.Run(context.Background(), sp["fish-s2-pro"], Request{Text: "Hello there, this is a test.", Voice: "Ryan"}, t.TempDir(), func(Event) {}); err == nil {
		t.Fatal("Ryan is no Fish S2 Pro voice")
	}
}
