package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/philipparndt/ai-connector/internal/audio"
	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// Speech: text to speech with a voice library (docs/voice-avatar.md), Qwen3-ASR for transcribing (voice samples,
// "transcribe" jobs), and Qwen3-ForcedAligner for the words' timings of every clip (when it is on disk). Every model
// runs in one Python worker (workers/speech.py) on mlx-audio.

// SpeechEnv() is the speech runtime, pinned (verified on an M1 Max, 64 GB).
func SpeechEnv() pyenv.Env { return runtimeEnv("speech") } // runtimes/speech.json

// speechFamily is what a model family can do with a request; see speechFamilies.
type speechFamily struct {
	presets  []string                 // the model's own voices
	preset   func(lang string) string // the default preset for a language ("" none: the model's own default voice)
	clone    bool                     // speaks with a voice of the library made from a sample
	design   bool                     // speaks with a voice of the library made from a description
	speed    bool                     // honours speed itself (else the wav is time-stretched afterwards, pitch kept)
	langs    []string                 // languages it needs to be told (nil: it detects them)
	honoured string                   // what of the request it honours, for the model's "about"
	words    bool                     // it gives word timings itself (Kokoro, English; any model with the aligner)
	speaks   []string                 // the languages it speaks when it is not told them (langs) — nil: many, it detects
	runtime  string                   // the Python runtime it runs in ("" the speech runtime: mlx-audio, speech.py)

	// a family a model's manifest describes (runtimeArgs.speech) instead of Go: mlx-audio's model type, and whether
	// it takes an instruction; speech.py runs it on its general path
	modelType string
	instruct  bool
}

var qwenPresets = []string{"Vivian", "Serena", "Uncle_Fu", "Dylan", "Eric", "Ryan", "Aiden", "Ono_Anna", "Sohee"}

var voxtralPresets = []string{"casual_male", "casual_female", "cheerful_female", "neutral_male", "neutral_female",
	"fr_male", "fr_female", "es_male", "es_female", "de_male", "de_female", "it_male", "it_female", "pt_male",
	"pt_female", "nl_male", "nl_female", "ar_male", "hi_male", "hi_female"}

// kokoroVoices are Kokoro v1.0's voices (voices/*.safetensors of the weights), Japanese left out (see SpeechEnv()).
// The first letter is the language, the second f(emale) or m(ale).
var kokoroVoices = []string{"af_heart", "af_alloy", "af_aoede", "af_bella", "af_jessica", "af_kore", "af_nicole",
	"af_nova", "af_river", "af_sarah", "af_sky", "am_adam", "am_echo", "am_eric", "am_fenrir", "am_liam", "am_michael",
	"am_onyx", "am_puck", "am_santa", "bf_alice", "bf_emma", "bf_isabella", "bf_lily", "bm_daniel", "bm_fable",
	"bm_george", "bm_lewis", "ef_dora", "em_alex", "em_santa", "ff_siwis", "hf_alpha", "hf_beta", "hm_omega", "hm_psi",
	"if_sara", "im_nicola", "pf_dora", "pm_alex", "pm_santa", "zf_xiaobei", "zf_xiaoni", "zf_xiaoxiao", "zf_xiaoyi",
	"zm_yunjian", "zm_yunxi", "zm_yunxia", "zm_yunyang"}

// kokoroLangs: Kokoro's voice prefixes as languages (a American, b British English).
var kokoroLangs = map[byte]string{'a': "en", 'b': "en", 'e': "es", 'f': "fr", 'h': "hi", 'i': "it", 'p': "pt", 'z': "zh"}

// kokoroLanguage is a Kokoro voice's language, and whether it is British English.
func kokoroLanguage(voice string) (string, bool) {
	if voice == "" {
		return "", false
	}
	return kokoroLangs[voice[0]], voice[0] == 'b'
}

// speechFamilies: the worker's model families (Model.workerModel).
var speechFamilies = map[string]*speechFamily{
	// Kokoro 82M: 49 preset voices in 7 languages (the voice's prefix); fast, small. Speed is its own; no instruct,
	// no cloning. English gives word timings.
	"kokoro": {presets: kokoroVoices, preset: func(lang string) string {
		if v, ok := map[string]string{"es": "ef_dora", "fr": "ff_siwis", "hi": "hf_alpha", "it": "if_sara", "pt": "pf_dora",
			"zh": "zf_xiaobei"}[lang]; ok {
			return v
		}
		return "af_heart"
	}, speed: true, langs: []string{"en", "es", "fr", "it", "pt", "hi", "zh"}, words: true,
		honoured: "49 preset voices in 7 languages (the voice sets the language), speed, word timings (English)"},
	// Qwen3-TTS: CustomVoice speaks the presets and follows instruct; Base clones (sample + transcript); VoiceDesign
	// makes a voice from its description. Language is passed (10 languages, German among them); speed by stretching.
	"qwen3-tts": {presets: qwenPresets, preset: func(string) string { return "Ryan" }, clone: true, design: true,
		honoured: "presets with instruct (emotion, style), cloning, voice design (1.7B), language",
		speaks:   []string{"zh", "en", "ja", "ko", "de", "fr", "ru", "pt", "es", "it"}},
	// Chatterbox Multilingual v3: one default voice (Resemble's), clones from a sample; instruct sets the emotion
	// exaggeration (a number 0–2, or words: calm … dramatic). Needs the language (23).
	"chatterbox": {presets: []string{"default"}, preset: func(string) string { return "default" }, clone: true,
		langs: []string{"ar", "da", "de", "el", "en", "es", "fi", "fr", "he", "hi", "it", "ja", "ko", "ms", "nl", "no",
			"pl", "pt", "ru", "sv", "sw", "tr", "zh"},
		honoured: "cloning, instruct as emotion exaggeration, language"},
	// VoxCPM2: no presets (a random voice per seed), clones (with the transcript: continuation, closer), designs;
	// instruct is a style description. Detects the language (30).
	"voxcpm2": {clone: true, design: true, honoured: "cloning, voice design, instruct as a style description; 48 kHz"},
	// Higgs Audio v3: no presets (a random voice per seed), clones; instruct becomes its control tags (emotion,
	// whisper, shout, speed, pitch) or is passed as tags (<|emotion:…|>); speed as tags.
	"higgs": {clone: true, speed: true, honoured: "cloning, instruct as control tags (emotions, whisper/shout), speed as tags"},
	// Voxtral TTS: 20 presets (two German); no cloning in the open weights, no instruct.
	"voxtral": {presets: voxtralPresets, preset: func(lang string) string {
		switch lang {
		case "de", "fr", "es", "it", "pt", "nl", "hi":
			return lang + "_female"
		case "ar":
			return "ar_male"
		}
		return "neutral_female"
	}, honoured: "presets (the language picks the default one)",
		speaks: []string{"en", "fr", "es", "de", "it", "pt", "nl", "ar", "hi"}},
	// Fish Audio S2 Pro (mlx-speech, its own runtime): no presets (a random voice per seed), clones from a sample with
	// its exact transcript (a sample in the text's language, else the accent carries over); instruct becomes an inline
	// tag ([whisper], [excited], [professional broadcast tone]), tags in the text are its own. Detects the language;
	// speed by stretching; no word timings (the aligner is in the other runtime).
	"fish-s2": {clone: true, runtime: "fish-speech",
		honoured: "cloning (sample + its transcript, same language), instruct as an inline [tag], [tags] in the text"},
	"asr":   {},
	"align": {},
}

// speechRuntime is the Python runtime a speech family runs in.
func speechRuntime(workerModel string) string {
	if f := speechFamilies[workerModel]; f != nil && f.runtime != "" {
		return f.runtime
	}
	return "speech"
}

// speechTool: the worker model is a tool of the speech models (the transcriber, the aligner), not a voice.
func speechTool(workerModel string) bool { return workerModel == "asr" || workerModel == "align" }

// SpeechPresets are the preset voices of each speech model (GET /api/voices): the built-in ones, and those of the
// catalogue c (a machine's own in models.d; nil: none).
func SpeechPresets(c *Catalog) map[string][]string {
	out := map[string][]string{}
	for id, p := range builtinPresets() {
		out[id] = append([]string{}, p...)
	}
	if c != nil {
		for _, m := range c.OfKind(Speech) {
			if _, ok := out[m.ID]; !ok { // the built-in ones are there: a machine's own only
				addPresets(out, m)
			}
		}
	}
	return out
}

// builtinPresets are the built-in speech models' presets, worked out once from their manifests (not per request).
var builtinPresets = sync.OnceValue(func() map[string][]string {
	out := map[string][]string{}
	for _, m := range speechModels() {
		addPresets(out, m)
	}
	return out
})

func addPresets(out map[string][]string, m *Model) {
	if f := speechFamilies[m.workerModel]; f != nil && !speechTool(m.workerModel) {
		out[m.ID] = append([]string{}, f.presets...)
	}
}

// asrRepo() is Qwen3-ASR 1.7B (8-bit): transcribes the voice samples and the "transcribe" jobs.
func asrRepo() hubRepo { return builtinRepo("qwen3-asr", "asr") }

// alignRepo() is Qwen3-ForcedAligner 0.6B (8-bit, the conversion mlx-audio names): the words' timings of every clip
// in its languages (alignLangs) once it is on disk — a model of its own, fetched by its download, never by a clip.
func alignRepo() hubRepo { return builtinRepo("qwen3-forced-aligner", "align") }

// alignLangs are the languages Qwen3-ForcedAligner times (its config's support_languages; not Japanese and Korean:
// mlx-audio splits their words with nagisa / soynlp, which the speech runtime does not have). workers/speech.py
// maps them to the aligner's names (ALIGN_LANGS).
var alignLangs = []string{"de", "en", "es", "fr", "it", "pt", "ru", "zh"}

// TaskAlign is the aligner's task: its own setup role ("speech/align"), beside the voices.
const TaskAlign = "align"

// speechModels are the catalogue's speech models.
func speechModels() []*Model { return builtinModels("speech", nil, "") }

// speechTasks are what a speech model honours, for the UI: clone, design, presets, instruct, speed, language (the
// transcriber: transcribe).
func speechTasks(m *Model) []string {
	switch m.workerModel {
	case "asr":
		return []string{"transcribe"}
	case "align":
		return []string{TaskAlign}
	}
	f := speechFamilies[m.workerModel]
	var t []string
	if f.clone {
		t = append(t, "clone")
	}
	if f.design && (m.workerModel != "qwen3-tts" || hasRepo(m, "design")) {
		t = append(t, "design")
	}
	if len(f.presets) > 0 {
		t = append(t, "presets")
	}
	if f.modelType != "" { // a family of a manifest: as it says
		if f.instruct {
			t = append(t, "instruct")
		}
	} else if m.workerModel != "voxtral" && m.workerModel != "kokoro" { // no instruction; the others take it (see speechFamilies)
		t = append(t, "instruct")
	}
	t = append(t, "speed") // the model's own (Higgs, Kokoro) or by time-stretching
	if m.workerModel == "qwen3-tts" || m.workerModel == "chatterbox" || m.workerModel == "voxtral" || m.workerModel == "kokoro" || (f.modelType != "" && len(f.langs) > 0) {
		t = append(t, "language") // told the language (Voxtral: it picks the default preset)
	}
	return t
}

// coreRepos are the repositories a model needs to count as here (Qwen3-TTS: its preset part; Base and VoiceDesign
// come when first used).
func coreRepos(m *Model) []hubRepo {
	if m.workerModel == "qwen3-tts" {
		return m.repos[:1]
	}
	return m.repos
}

// reposHere: the store has every file of these repositories.

func (hr hubRepo) keep() func(string) bool {
	if len(hr.include) == 0 {
		return nil
	}
	inc := hr.include
	return func(p string) bool { return models.Matches(p, inc...) }
}

// VoiceStore looks voices of the library up (internal/voices implements it).
type VoiceStore interface {
	Voice(id string) (Voice, bool)
}

// SpeechBackend runs the speech models in one mlx-audio worker (a family of its own runtime, Fish S2 Pro's
// mlx-speech, in a worker of that runtime: one of them runs at a time); it keeps the last TTS model (and Qwen3-ASR,
// once used) loaded. It is the Speaker of the avatar backend.
type SpeechBackend struct {
	Root    string
	Store   *models.Store
	Catalog *Catalog   // Speak looks its model up here
	Voices  VoiceStore // the voice library (nil: presets only)

	// tests: a fake runtime, and the repositories' folders without fetching
	env        *pyenv.Env
	fetch      func(ctx context.Context, name string, hr hubRepo, emit Emit) (string, error)
	hasAligner func() bool // Qwen3-ForcedAligner on disk (nil: the store says)

	mu    sync.Mutex
	ws    map[string]*pyWorker // by runtime (speechRuntime)
	state status
}

var _ Speaker = (*SpeechBackend)(nil)

func (b *SpeechBackend) Name() string   { return "speech" }
func (b *SpeechBackend) Heavy() bool    { return true }
func (b *SpeechBackend) Loaded() string { return b.state.get() }

func (b *SpeechBackend) Unload() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range b.ws {
		w.stop()
	}
	b.state.set("")
}

// Run says r.Text into a wav in outDir, or (qwen3-asr) transcribes r.Audio.
func (b *SpeechBackend) Run(ctx context.Context, m *Model, r Request, outDir string, emit Emit) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if m.workerModel == "asr" {
		if r.Audio == "" {
			return errors.New("an audio file to transcribe")
		}
		if _, err := os.Stat(r.Audio); err != nil {
			return fmt.Errorf("the audio to transcribe: %w", err)
		}
		t0 := time.Now()
		text, err := b.transcribe(ctx, r.Audio, r.Language, emit)
		if err != nil {
			return err
		}
		emit(Event{Type: "text", Text: text})
		emit(Event{Type: "result", Data: map[string]any{"text": text}})
		emit(Event{Type: "stats", Data: map[string]any{"seconds": time.Since(t0).Seconds(), "duration": wavSeconds(r.Audio)}})
		return nil
	}
	if r.Seed == 0 {
		r.Seed = rand.Int64N(1_000_000_000)
	}
	out := filepath.Join(outDir, fmt.Sprintf("%s-%d.wav", time.Now().Format("20060102-150405"), r.Seed))
	said, err := b.speak(ctx, m, r, out, emit)
	if err != nil {
		return err
	}
	emit(Event{Type: "file", File: out})
	// what was said, with which voice and language, and when each word is (a model that gives timings)
	res := map[string]any{"text": said.text, "language": said.language}
	if said.voice.ID != "" {
		res["voice"] = said.voice
	}
	if len(said.words) > 0 {
		res["words"] = said.words
	}
	emit(Event{Type: "result", Data: res})
	return nil
}

// spoken is what speak said: the text, the voice (a preset or a voice of the library; none: the model's own),
// the language, and the words' timings when the model gives them.
type spoken struct {
	text, language string
	voice          PresetVoice
	words          []Word
}

// Word is a spoken word and when it is said (seconds from the start).
type Word struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Speak says r.Text with r.Voice into outFile, in this process (the avatar backend runs it inside its own job).
// model "" is the first speech model on this machine (else Qwen3-TTS 1.7B).
func (b *SpeechBackend) Speak(ctx context.Context, model string, r Request, outFile string, emit Emit) error {
	m, err := b.speechModel(model)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Seed == 0 {
		r.Seed = rand.Int64N(1_000_000_000)
	}
	_, err = b.speak(ctx, m, r, outFile, emit)
	return err
}

func (b *SpeechBackend) speechModel(id string) (*Model, error) {
	var all []*Model
	if b.Catalog != nil {
		all = b.Catalog.OfKind(Speech)
	} else {
		all = speechModels()
	}
	var tts []*Model
	for _, m := range all {
		if m.Backend == "speech" && !speechTool(m.workerModel) {
			tts = append(tts, m)
		}
	}
	if id == "" {
		for _, m := range tts {
			if m.Here {
				return m, nil
			}
		}
		for _, m := range tts {
			if m.ID == "qwen3-tts-1.7b" {
				return m, nil
			}
		}
	}
	for _, m := range tts {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, fmt.Errorf("no speech model %q", id)
}

// PreviewVoice says a short text with a designed voice of the library (Qwen3-TTS 1.7B VoiceDesign) into out: the
// library keeps it as the voice's sample.
func (b *SpeechBackend) PreviewVoice(ctx context.Context, v Voice, out string) error {
	text := "Hello! This is how my voice sounds. I hope you like it."
	if v.Language == "de" {
		text = "Hallo! So klingt meine Stimme. Ich hoffe, sie gefällt dir."
	}
	return b.Speak(ctx, "qwen3-tts-1.7b", Request{Text: text, Voice: v.ID, Language: v.Language}, out, func(Event) {})
}

// Transcribe writes down what an audio file says (Qwen3-ASR; language "" detects it).
func (b *SpeechBackend) Transcribe(ctx context.Context, file, language string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.transcribe(ctx, file, language, func(Event) {})
}

func (b *SpeechBackend) transcribe(ctx context.Context, file, language string, emit Emit) (string, error) {
	dir, err := b.fetchRepo(ctx, "Qwen3-ASR", asrRepo(), emit)
	if err != nil {
		return "", err
	}
	w, err := b.worker(ctx, "speech", emit)
	if err != nil {
		return "", err
	}
	if b.state.get() == "" {
		b.state.set("qwen3-asr")
	}
	ev, err := w.call(ctx, map[string]any{"op": "asr", "dirs": map[string]string{"asr": dir}, "audio": file,
		"language": language}, emit)
	if err != nil {
		b.afterError(w)
		return "", err
	}
	return strings.TrimSpace(ev.Text), nil
}

// fetchRepo makes sure one pinned repository is in the store and returns its folder.
func (b *SpeechBackend) fetchRepo(ctx context.Context, name string, hr hubRepo, emit Emit) (string, error) {
	if b.fetch != nil {
		return b.fetch(ctx, name, hr, emit)
	}
	hm, err := b.Store.FromHub(ctx, name, hr.repo, hr.rev, hr.keep())
	if errors.Is(err, models.ErrGated) {
		return "", GatedError(hr.repo)
	}
	if err != nil {
		return "", err
	}
	return fetch(ctx, b.Store, hm, emit)
}

// worker is the running worker of a runtime ("speech", or a family's own): the other runtime's worker is stopped
// first, so only one of them keeps a model in memory.
func (b *SpeechBackend) worker(ctx context.Context, runtime string, emit Emit) (*pyWorker, error) {
	for name, w := range b.ws {
		if name != runtime && w.running() {
			emit.Log("stopping the " + name + " runtime (one speech runtime at a time)")
			w.stop()
			b.state.set("")
		}
	}
	w := b.ws[runtime]
	if w == nil {
		env := runtimeEnv(runtime)
		if b.env != nil {
			env = *b.env
		}
		script := "speech.py"
		if rt := runtimeManifest(runtime); rt != nil && rt.Script != "" {
			script = rt.Script
		}
		w = &pyWorker{env: env, script: script}
		if b.ws == nil {
			b.ws = map[string]*pyWorker{}
		}
		b.ws[runtime] = w
	}
	if w.running() {
		return w, nil
	}
	b.state.set("")
	return w, w.start(ctx, b.Root, emit)
}

func (b *SpeechBackend) afterError(w *pyWorker) {
	if w == nil || !w.running() {
		b.state.set("")
	}
}

// speak does the work of Run and Speak (b.mu held).
func (b *SpeechBackend) speak(ctx context.Context, m *Model, r Request, out string, emit Emit) (spoken, error) {
	var said spoken
	fam := speechFamilies[m.workerModel]
	if fam == nil || speechTool(m.workerModel) {
		return said, fmt.Errorf("%s does not speak", m.Name)
	}
	text := strings.TrimSpace(r.Text)
	if text == "" {
		text = strings.TrimSpace(r.Prompt)
	}
	if text == "" {
		return said, errors.New("a text to say")
	}
	v, err := resolveVoice(m, b.Voices, r.Voice)
	if err != nil {
		return said, err
	}
	lang := strings.ToLower(strings.TrimSpace(r.Language))
	if lang == "" {
		lang = v.language
	}
	if lang == "" {
		lang = guessLanguage(text)
	}
	if v.Mode == "default" && fam.preset != nil {
		v.Mode, v.Preset = "preset", fam.preset(lang)
	}
	if fam.langs != nil && !slices.Contains(fam.langs, lang) {
		if r.Language != "" {
			return said, fmt.Errorf("%s does not speak %q (it speaks: %s)", m.Name, lang, strings.Join(fam.langs, ", "))
		}
		lang = "en"
	}
	chunks := splitSentences(text)

	// the repositories: Qwen3-TTS needs the part of its voice (presets: CustomVoice, clone: Base, design: VoiceDesign)
	dirs := map[string]string{}
	for _, hr := range m.repos {
		key := hr.key
		if m.workerModel == "qwen3-tts" {
			if hr.key != qwenPart(v.Mode) {
				continue
			}
			key = "model"
		}
		dir, err := b.fetchRepo(ctx, m.Name, hr, emit)
		if err != nil {
			return said, err
		}
		dirs[key] = dir
	}
	if dirs["model"] == "" {
		return said, fmt.Errorf("%s cannot speak with a %s voice", m.Name, v.Mode)
	}
	// the words' timings: Qwen3-ForcedAligner when it is on disk and times the language (never fetched for a clip);
	// it runs in the speech runtime, not in a family's own
	if fam.runtime == "" && slices.Contains(alignLangs, lang) && b.alignerHere() {
		if dir, err := b.fetchRepo(ctx, "Qwen3-ForcedAligner", alignRepo(), emit); err == nil {
			dirs["align"] = dir
		} else {
			emit.Log("no word timings: " + err.Error())
		}
	}
	w, err := b.worker(ctx, speechRuntime(m.workerModel), emit)
	if err != nil {
		return said, err
	}
	req := map[string]any{"op": "tts", "family": m.workerModel, "dirs": dirs, "voice": v, "instruct": r.Instruct,
		"language": lang, "seed": r.Seed, "chunks": chunks, "output": out}
	if m.workerModel == "chatterbox" {
		req["exaggeration"] = exaggeration(r.Instruct)
	}
	if fam.modelType != "" { // a manifest's family: speech.py's general path
		req["modelType"], req["langCode"], req["takesInstruct"] = fam.modelType, len(fam.langs) > 0, fam.instruct
	}
	if fam.speed {
		req["speed"] = r.Speed
	}
	emit(Event{Type: "stats", Data: map[string]any{"seed": r.Seed, "voice": v.label(), "sentences": len(chunks), "language": lang}})
	emit.Log(m.Name + " honours: " + fam.honoured)
	b.state.set(m.ID) // loading counts as loaded: it takes the memory
	ev, err := w.call(ctx, req, emit)
	if err != nil {
		b.afterError(w)
		return said, err
	}
	if !fam.speed && r.Speed > 0 && (r.Speed < 0.99 || r.Speed > 1.01) {
		emit.Status(fmt.Sprintf("speed ×%.2f …", r.Speed))
		if err := stretch(ctx, out, r.Speed); err != nil {
			return said, err
		}
		ev.Words = stretchWords(ev.Words, r.Speed)
	}
	rate, dur := wavInfo(out)
	stats := map[string]any{"seconds": ev.Seconds, "duration": dur, "sampleRate": rate}
	if dur > 0 {
		stats["rtf"] = ev.Seconds / dur
	}
	if ev.PeakGB > 0 {
		stats["peakGB"] = ev.PeakGB
	}
	emit(Event{Type: "stats", Data: stats})
	emit.Log(fmt.Sprintf("%.1f s of speech in %.1f s", dur, ev.Seconds))
	said.text, said.language, said.words = text, lang, ev.Words
	switch {
	case v.Mode == "preset":
		said.voice = presetVoice(m.workerModel, v.Preset)
	case v.name != "":
		said.voice = PresetVoice{ID: strings.TrimSpace(r.Voice), Name: v.name, Language: v.language}
	}
	return said, nil
}

// alignerHere: Qwen3-ForcedAligner is on disk (a clip uses it then; it never fetches it).
func (b *SpeechBackend) alignerHere() bool {
	if b.hasAligner != nil {
		return b.hasAligner()
	}
	return b.Store != nil && reposHere(b.Store, []hubRepo{alignRepo()})
}

// stretchWords moves the words' timings with the clip's speed change (the wav stretched to 1/speed of its length).
func stretchWords(words []Word, speed float64) []Word {
	speed = min(max(speed, 0.5), 2) // as stretch
	for i := range words {
		words[i].Start = math.Round(words[i].Start/speed*1000) / 1000
		words[i].End = math.Round(words[i].End/speed*1000) / 1000
	}
	return words
}

// qwenPart is the Qwen3-TTS checkpoint a voice needs.
func qwenPart(mode string) string {
	switch mode {
	case "clone":
		return "base"
	case "design":
		return "design"
	}
	return "custom"
}

// speechVoice is a request's voice as the worker gets it.
type speechVoice struct {
	Mode        string `json:"mode"` // preset | clone | design | default (the model's own voice)
	Preset      string `json:"preset,omitempty"`
	RefAudio    string `json:"ref_audio,omitempty"`
	RefText     string `json:"ref_text,omitempty"`
	Description string `json:"description,omitempty"`
	name        string // the library voice's name
	language    string // the library voice's language
}

func (v speechVoice) label() string {
	switch {
	case v.name != "":
		return v.name
	case v.Preset != "":
		return v.Preset
	}
	return v.Mode
}

// resolveVoice: a voice of the library (by id, else by name) → its sample and transcript (clone) or description
// (design); else one of the model's presets (case does not matter); "" → the model's default.
func resolveVoice(m *Model, lib VoiceStore, name string) (speechVoice, error) {
	fam := speechFamilies[m.workerModel]
	name = strings.TrimSpace(name)
	if name == "" || strings.EqualFold(name, "default") {
		return speechVoice{Mode: "default"}, nil
	}
	if lib != nil {
		if lv, ok := lib.Voice(name); ok {
			sv := speechVoice{Mode: lv.Kind, name: lv.Name, language: lv.Language}
			switch lv.Kind {
			case "clone":
				if !fam.clone {
					return sv, fmt.Errorf("%s cannot clone a voice: pick one of its presets (%s)", m.Name, strings.Join(fam.presets, ", "))
				}
				if _, err := os.Stat(lv.Sample); err != nil {
					return sv, fmt.Errorf("the sample of voice %q is missing: %w", lv.Name, err)
				}
				sv.RefAudio, sv.RefText = lv.Sample, lv.Transcript
			case "design":
				if !fam.design || (m.workerModel == "qwen3-tts" && !hasRepo(m, "design")) {
					return sv, fmt.Errorf("%s cannot make a voice from a description (Qwen3-TTS 1.7B and VoxCPM2 can)", m.Name)
				}
				sv.Description = lv.Description
			default:
				return sv, fmt.Errorf("voice %q: unknown kind %q", lv.Name, lv.Kind)
			}
			return sv, nil
		}
	}
	for _, p := range fam.presets {
		if strings.EqualFold(p, name) {
			if p == "default" {
				return speechVoice{Mode: "default"}, nil
			}
			sv := speechVoice{Mode: "preset", Preset: p}
			if m.workerModel == "kokoro" { // the voice is made for one language: it is the default
				sv.language, _ = kokoroLanguage(p)
			}
			return sv, nil
		}
	}
	if len(fam.presets) == 0 {
		return speechVoice{}, fmt.Errorf("no voice %q: %s has no preset voices, use one of the library", name, m.Name)
	}
	return speechVoice{}, fmt.Errorf("no voice %q (not in the library; %s's presets: %s)", name, m.Name, strings.Join(fam.presets, ", "))
}

func hasRepo(m *Model, key string) bool {
	return slices.ContainsFunc(m.repos, func(hr hubRepo) bool { return hr.key == key })
}

// exaggeration maps an instruction onto Chatterbox's emotion exaggeration (0.5 neutral … 2 extreme): a number as it
// is, else words.
func exaggeration(instruct string) float64 {
	s := strings.ToLower(strings.TrimSpace(instruct))
	if s == "" {
		return 0.5
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return min(max(f, 0), 2)
	}
	for _, w := range []struct {
		words []string
		v     float64
	}{
		{[]string{"extreme", "furious", "hysteri", "screaming", "shouting", "over the top", "very dramatic"}, 1.3},
		{[]string{"dramatic", "excited", "angry", "emotional", "passionate", "energetic", "enthusias", "intense"}, 0.9},
		{[]string{"lively", "happy", "cheerful", "expressive", "amused", "warm"}, 0.7},
		{[]string{"monoton", "flat", "robotic"}, 0.25},
		{[]string{"calm", "neutral", "soft", "quiet", "gentle", "relaxed", "slow", "serious"}, 0.35},
	} {
		for _, k := range w.words {
			if strings.Contains(s, k) {
				return w.v
			}
		}
	}
	return 0.5
}

// guessLanguage is a text's language where it is easy to tell (for the models that must be told; "" when it cannot tell).
func guessLanguage(text string) string { return audio.GuessLanguage(text) }

// speechChunk is one piece the model says at once, and the silence after it.
type speechChunk struct {
	Text  string  `json:"text"`
	Pause float64 `json:"pause"`
}

const (
	chunkMin       = 24  // a shorter sentence goes with the next one ("Hallo!" alone sounds clipped)
	chunkMax       = 280 // a longer one is split at commas (the models drift on long inputs)
	pauseSentence  = 0.25
	pauseLine      = 0.4
	pauseParagraph = 0.7
)

// splitSentences splits a text into sentences for saying one at a time (a line break and a paragraph end one too); the
// worker joins them with short pauses: longer after a line, longer still after a paragraph.
func splitSentences(text string) []speechChunk {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var out []speechChunk
	for _, para := range splitParagraphs(text) {
		for _, line := range strings.Split(para, "\n") {
			line = strings.Join(strings.Fields(line), " ")
			if line == "" {
				continue
			}
			var lineChunks []string
			pending := ""
			for _, s := range sentences(line) {
				if pending != "" {
					s, pending = pending+" "+s, ""
				}
				if utf8.RuneCountInString(s) < chunkMin {
					pending = s
					continue
				}
				lineChunks = append(lineChunks, splitLong(s, chunkMax)...)
			}
			if pending != "" { // a short tail joins the sentence before it, else stands alone
				if n := len(lineChunks); n > 0 && utf8.RuneCountInString(lineChunks[n-1]+pending) < chunkMax {
					lineChunks[n-1] += " " + pending
				} else {
					lineChunks = append(lineChunks, pending)
				}
			}
			for _, c := range lineChunks {
				out = append(out, speechChunk{Text: c, Pause: pauseSentence})
			}
			out[len(out)-1].Pause = pauseLine
		}
		if n := len(out); n > 0 {
			out[n-1].Pause = pauseParagraph
		}
	}
	if n := len(out); n > 0 {
		out[n-1].Pause = 0
	}
	return out
}

func splitParagraphs(text string) []string {
	var out, cur []string
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == "" {
			if len(cur) > 0 {
				out = append(out, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, l)
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, "\n"))
	}
	return out
}

// abbreviations end in a dot without ending the sentence (lower case, inner dots kept).
var abbreviations = map[string]bool{}

func init() {
	for _, a := range strings.Fields(`dr prof hr hrn fr frl nr st str ca bzw usw vgl ggf evtl bspw bzgl inkl exkl zzgl
		z.b d.h u.a o.ä u.u s.o s.u v.a mr mrs ms jr sr vs etc e.g i.e inc ltd co no approx dept fig jan feb mar apr jun
		jul aug sep sept oct okt nov dec dez mio mrd tel abs art bd geb gest dipl ing mag dt engl franz lt gem min max
		ff hl mt sog`) {
		abbreviations[a] = true
	}
}

var months = map[string]bool{"januar": true, "februar": true, "märz": true, "april": true, "mai": true, "juni": true,
	"juli": true, "august": true, "september": true, "oktober": true, "november": true, "dezember": true}

const (
	sentenceMarks = ".!?…"
	closingMarks  = `"'»«“”„‘’)]`
)

// sentences splits one line at sentence ends: . ! ? … (and closing quotes), not inside numbers or addresses, not
// after abbreviations, initials or ordinals ("am 3. Oktober").
func sentences(line string) []string {
	rs := []rune(line)
	var out []string
	start := 0
	for i := 0; i < len(rs); i++ {
		if !strings.ContainsRune(sentenceMarks, rs[i]) {
			continue
		}
		j := i
		for j+1 < len(rs) && strings.ContainsRune(sentenceMarks, rs[j+1]) {
			j++
		}
		k := j
		for k+1 < len(rs) && strings.ContainsRune(closingMarks, rs[k+1]) {
			k++
		}
		if k+1 < len(rs) && !unicode.IsSpace(rs[k+1]) {
			i = j
			continue // 3.5, www.example.org
		}
		if j == i && rs[i] == '.' && !sentenceEnd(rs[start:i], nextWord(rs[k+1:])) {
			i = j
			continue
		}
		if s := strings.TrimSpace(string(rs[start : k+1])); s != "" {
			out = append(out, s)
		}
		start, i = k+1, k
	}
	if s := strings.TrimSpace(string(rs[start:])); s != "" {
		out = append(out, s)
	}
	return out
}

func nextWord(rs []rune) string {
	if f := strings.Fields(string(rs)); len(f) > 0 {
		return f[0]
	}
	return ""
}

// sentenceEnd: does the dot after before end the sentence (next: the word after it)?
func sentenceEnd(before []rune, next string) bool {
	f := strings.Fields(string(before))
	if next == "" || len(f) == 0 {
		return true
	}
	word := strings.TrimLeft(f[len(f)-1], closingMarks+"([")
	if abbreviations[strings.ToLower(word)] {
		return false
	}
	if rs := []rune(word); len(rs) == 1 && unicode.IsLetter(rs[0]) {
		return false // an initial: J. R. R. Tolkien, z. B.
	}
	nr, _ := utf8.DecodeRuneInString(strings.TrimLeft(next, closingMarks+"(["))
	if allDigits(word) && len(word) <= 2 && (unicode.IsLower(nr) || months[strings.ToLower(strings.Trim(next, ",.;:"))]) {
		return false // an ordinal: am 3. Oktober
	}
	return !unicode.IsLower(nr)
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// splitLong splits a sentence longer than max runes at its last clause mark (, ; : – —) that fits, else between
// words.
func splitLong(s string, max int) []string {
	var out, cur []string
	n := 0
	for _, w := range strings.Fields(s) {
		wl := utf8.RuneCountInString(w) + 1
		if n+wl > max && len(cur) > 0 {
			cut := len(cur) - 1 // between words
			for k := len(cur) - 1; k > 0; k-- {
				if strings.HasSuffix(cur[k], ",") || strings.HasSuffix(cur[k], ";") || strings.HasSuffix(cur[k], ":") || cur[k] == "–" || cur[k] == "—" {
					cut = k
					break
				}
			}
			out = append(out, strings.Join(cur[:cut+1], " "))
			cur = append([]string{}, cur[cut+1:]...)
			n = 0
			for _, r := range cur {
				n += utf8.RuneCountInString(r) + 1
			}
		}
		cur = append(cur, w)
		n += wl
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, " "))
	}
	return out
}

// stretch changes the speed of a wav, keeping the pitch (ffmpeg atempo, 0.5–2).
func stretch(ctx context.Context, file string, speed float64) error {
	speed = min(max(speed, 0.5), 2)
	tmp := strings.TrimSuffix(file, ".wav") + ".tempo.wav"
	out, err := exec.CommandContext(ctx, "ffmpeg", "-y", "-v", "error", "-i", file, "-filter:a",
		"atempo="+strconv.FormatFloat(speed, 'f', 3, 64), tmp).CombinedOutput()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("changing the speed (ffmpeg): %v %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, file)
}

// wavInfo reads a PCM wav's sample rate and length from its header.
func wavInfo(file string) (rate int, seconds float64) {
	f, err := os.Open(file)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	var hdr [12]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil || string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return 0, 0
	}
	var byteRate uint32
	for {
		var ch [8]byte
		if _, err := io.ReadFull(f, ch[:]); err != nil {
			return rate, 0
		}
		size := binary.LittleEndian.Uint32(ch[4:8])
		switch string(ch[0:4]) {
		case "fmt ":
			b := make([]byte, size+size%2)
			if _, err := io.ReadFull(f, b); err != nil || len(b) < 12 {
				return 0, 0
			}
			rate, byteRate = int(binary.LittleEndian.Uint32(b[4:8])), binary.LittleEndian.Uint32(b[8:12])
		case "data":
			if byteRate == 0 {
				return rate, 0
			}
			if size == 0 || size == 0xffffffff { // streamed: the rest of the file
				if st, err := f.Stat(); err == nil {
					pos, _ := f.Seek(0, io.SeekCurrent)
					size = uint32(st.Size() - pos)
				}
			}
			return rate, float64(size) / float64(byteRate)
		default:
			if _, err := f.Seek(int64(size+size%2), io.SeekCurrent); err != nil {
				return rate, 0
			}
		}
	}
}

// wavSeconds is a sound file's length (the wav header; ffprobe for other formats).
func wavSeconds(file string) float64 {
	if _, s := wavInfo(file); s > 0 {
		return s
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", file).Output()
	if err != nil {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return f
}

// manifestFamily is a speech family a model's manifest describes (runtimeArgs.speech): mlx-audio runs it on
// speech.py's general path.
func manifestFamily(name, modelType string, voices []string, def string, langs, speaks []string, clone, instruct, speed bool) *speechFamily {
	if def == "" && len(voices) > 0 {
		def = voices[0]
	}
	var said []string
	if len(voices) > 0 {
		said = append(said, fmt.Sprintf("%d preset voices", len(voices)))
	}
	if clone {
		said = append(said, "cloning")
	}
	if instruct {
		said = append(said, "instruct")
	}
	if speed {
		said = append(said, "speed")
	}
	if len(langs) > 0 {
		said = append(said, "language")
	}
	f := &speechFamily{presets: voices, clone: clone, speed: speed, langs: langs, speaks: speaks, modelType: modelType,
		instruct: instruct, honoured: strings.Join(said, ", ") + " (" + name + ", described by its manifest)"}
	if def != "" {
		f.preset = func(string) string { return def }
	}
	return f
}
