package engine

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSentences(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"Guten Abend! Das ist ein Test. Wirklich?", []string{"Guten Abend!", "Das ist ein Test.", "Wirklich?"}},
		{"Das kostet z. B. 3,50 € bzw. etwas mehr. Danach gehen wir.", []string{"Das kostet z. B. 3,50 € bzw. etwas mehr.", "Danach gehen wir."}},
		{"Wir treffen uns am 3. Oktober bei Dr. Müller. Er kommt um 10 Uhr.", []string{"Wir treffen uns am 3. Oktober bei Dr. Müller.", "Er kommt um 10 Uhr."}},
		{"Das war im Jahr 2024. Danach kam 2025.", []string{"Das war im Jahr 2024.", "Danach kam 2025."}},
		{"Version 3.5 is out, see www.example.org. Great!", []string{"Version 3.5 is out, see www.example.org.", "Great!"}},
		{"„Wer bist du?“, fragte der Fuchs. „Ein Stern!“ Er lachte.", []string{"„Wer bist du?“, fragte der Fuchs.", "„Ein Stern!“", "Er lachte."}},
		{"J. R. R. Tolkien wrote it... Really?! Yes.", []string{"J. R. R. Tolkien wrote it...", "Really?!", "Yes."}},
		{"Mr. Smith arrived, e.g. at noon. It rained", []string{"Mr. Smith arrived, e.g. at noon.", "It rained"}},
	} {
		if got := sentences(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("sentences(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestSplitSentences(t *testing.T) {
	got := splitSentences("Guten Abend! Das ist ein Test der lokalen Sprachausgabe.")
	want := []speechChunk{{Text: "Guten Abend! Das ist ein Test der lokalen Sprachausgabe.", Pause: 0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("short sentence joins the next: %+v", got)
	}

	got = splitSentences("Erster Satz ist lang genug. Zweiter Satz ist auch lang genug. Ja.\nNeue Zeile mit Inhalt hier.\n\n\nNeuer Absatz beginnt hier.")
	want = []speechChunk{
		{Text: "Erster Satz ist lang genug.", Pause: pauseSentence},
		{Text: "Zweiter Satz ist auch lang genug. Ja.", Pause: pauseLine}, // a short tail joins the sentence before
		{Text: "Neue Zeile mit Inhalt hier.", Pause: pauseParagraph},
		{Text: "Neuer Absatz beginnt hier.", Pause: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}

	if got := splitSentences("  \n\n "); len(got) != 0 {
		t.Fatalf("empty text: %+v", got)
	}

	// a long sentence is cut at a comma, never above the limit, and nothing is lost
	long := strings.Repeat("ein Wort nach dem anderen, ", 30) + "und am Ende ein Punkt."
	got = splitSentences(long)
	if len(got) < 2 {
		t.Fatalf("long sentence not split: %d chunks", len(got))
	}
	var words []string
	for _, c := range got {
		if n := len([]rune(c.Text)); n > chunkMax {
			t.Fatalf("chunk of %d runes: %q", n, c.Text)
		}
		if !strings.HasSuffix(c.Text, ",") && !strings.HasSuffix(c.Text, ".") {
			t.Fatalf("not cut at a clause mark: %q", c.Text)
		}
		words = append(words, strings.Fields(c.Text)...)
	}
	if strings.Join(words, " ") != strings.Join(strings.Fields(long), " ") {
		t.Fatal("words lost or changed")
	}
	// no clause marks at all: cut between words
	for _, c := range splitLong(strings.Repeat("Wort ", 200), 50) {
		if len(c) > 50 {
			t.Fatalf("chunk %q", c)
		}
	}
}

type fakeVoices map[string]Voice

func (f fakeVoices) Voice(id string) (Voice, bool) { v, ok := f[id]; return v, ok }

func TestResolveVoice(t *testing.T) {
	sample := filepath.Join(t.TempDir(), "s.wav")
	if err := os.WriteFile(sample, []byte("RIFF"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := fakeVoices{
		"anna-1": {ID: "anna-1", Name: "Anna", Kind: "clone", Sample: sample, Transcript: "Hallo.", Language: "de"},
		"gone-1": {ID: "gone-1", Name: "Gone", Kind: "clone", Sample: sample + ".missing"},
		"oma-1":  {ID: "oma-1", Name: "Oma", Kind: "design", Description: "a calm older woman"},
	}
	byID := map[string]*Model{}
	for _, m := range speechModels() {
		byID[m.ID] = m
	}
	for _, c := range []struct {
		model, voice string
		want         speechVoice
		err          string
	}{
		{"qwen3-tts-1.7b", "", speechVoice{Mode: "default"}, ""},
		{"qwen3-tts-1.7b", "ryan", speechVoice{Mode: "preset", Preset: "Ryan"}, ""},
		{"qwen3-tts-1.7b", "anna-1", speechVoice{Mode: "clone", RefAudio: sample, RefText: "Hallo.", name: "Anna", language: "de"}, ""},
		{"qwen3-tts-1.7b", "oma-1", speechVoice{Mode: "design", Description: "a calm older woman", name: "Oma"}, ""},
		{"qwen3-tts-0.6b", "oma-1", speechVoice{}, "cannot make a voice from a description"},
		{"qwen3-tts-1.7b", "gone-1", speechVoice{}, "sample of voice \"Gone\" is missing"},
		{"qwen3-tts-1.7b", "nobody", speechVoice{}, "presets: Vivian"},
		{"voxtral-tts", "anna-1", speechVoice{}, "cannot clone"},
		{"voxtral-tts", "DE_MALE", speechVoice{Mode: "preset", Preset: "de_male"}, ""},
		{"chatterbox-v3", "default", speechVoice{Mode: "default"}, ""},
		{"chatterbox-v3", "anna-1", speechVoice{Mode: "clone", RefAudio: sample, RefText: "Hallo.", name: "Anna", language: "de"}, ""},
		{"chatterbox-v3", "oma-1", speechVoice{}, "cannot make a voice"},
		{"voxcpm2", "oma-1", speechVoice{Mode: "design", Description: "a calm older woman", name: "Oma"}, ""},
		{"higgs-audio-v3", "ryan", speechVoice{}, "has no preset voices"},
		{"fish-s2-pro", "anna-1", speechVoice{Mode: "clone", RefAudio: sample, RefText: "Hallo.", name: "Anna", language: "de"}, ""},
		{"fish-s2-pro", "oma-1", speechVoice{}, "cannot make a voice"},
		{"fish-s2-pro", "ryan", speechVoice{}, "has no preset voices"},
	} {
		got, err := resolveVoice(byID[c.model], lib, c.voice)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s/%s: error %v, want %q", c.model, c.voice, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s/%s: %+v %v, want %+v", c.model, c.voice, got, err, c.want)
		}
	}
	// no library: presets only
	if _, err := resolveVoice(byID["qwen3-tts-1.7b"], nil, "anna-1"); err == nil {
		t.Error("a library voice without a library")
	}
}

func TestSpeechCatalog(t *testing.T) {
	presets := SpeechPresets(nil)
	if _, ok := presets["qwen3-asr"]; ok {
		t.Error("the transcriber has no voices")
	}
	if _, ok := presets["qwen3-forced-aligner"]; ok {
		t.Error("the aligner has no voices")
	}
	if len(presets["voxtral-tts"]) != 20 || len(presets["qwen3-tts-1.7b"]) != 9 || len(presets["kokoro-82m"]) != 49 {
		t.Errorf("presets %v", presets)
	}
	for _, m := range speechModels() {
		if m.Licence == "" {
			t.Errorf("%s: no licence", m.ID)
		}
		if speechFamilies[m.workerModel] == nil {
			t.Errorf("%s: no family %q", m.ID, m.workerModel)
		}
		for _, hr := range m.repos {
			if len(hr.rev) != 40 {
				t.Errorf("%s: %s not pinned", m.ID, hr.repo)
			}
		}
		nc := m.ID == "higgs-audio-v3" || m.ID == "voxtral-tts" || m.ID == "fish-s2-pro"
		if m.NonCommercial != nc {
			t.Errorf("%s: nonCommercial %v", m.ID, m.NonCommercial)
		}
	}
	tasks := map[string]string{}
	for _, m := range speechModels() {
		tasks[m.ID] = strings.Join(m.Tasks, ",")
	}
	for id, want := range map[string]string{
		"qwen3-tts-1.7b":       "clone,design,presets,instruct,speed,language",
		"qwen3-tts-0.6b":       "clone,presets,instruct,speed,language",
		"chatterbox-v3":        "clone,presets,instruct,speed,language",
		"voxcpm2":              "clone,design,instruct,speed",
		"higgs-audio-v3":       "clone,instruct,speed",
		"voxtral-tts":          "presets,speed,language",
		"kokoro-82m":           "presets,speed,language",
		"fish-s2-pro":          "clone,instruct,speed",
		"qwen3-asr":            "transcribe",
		"qwen3-forced-aligner": "align",
	} {
		if tasks[id] != want {
			t.Errorf("%s tasks %q, want %q", id, tasks[id], want)
		}
	}
	al := byID(speechModels())["qwen3-forced-aligner"]
	if al == nil || len(al.repos) != 1 || al.repos[0].rev != alignRepo().rev || al.Licence != "Apache-2.0" || al.Size < gb || al.Size > 2*gb ||
		alignRepo().repo != "mlx-community/Qwen3-ForcedAligner-0.6B-8bit" {
		t.Errorf("aligner %+v", al)
	}
	if _, err := (&SpeechBackend{}).speechModel("qwen3-forced-aligner"); err == nil {
		t.Error("the aligner speaks")
	}
	if qwenPart("clone") != "base" || qwenPart("design") != "design" || qwenPart("preset") != "custom" {
		t.Error("qwen parts")
	}
}

func TestExaggerationAndLanguage(t *testing.T) {
	for in, want := range map[string]float64{"": 0.5, "0.8": 0.8, "5": 2, "very dramatic!": 1.3, "ruhig, calm": 0.35, "excited": 0.9, "something": 0.5} {
		if got := exaggeration(in); got != want {
			t.Errorf("exaggeration(%q) = %v, want %v", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"Guten Abend! Das ist ein Test der lokalen Sprachausgabe.": "de",
		"Good evening, this is a test of the speech output.":       "en",
		"Grüße": "de",
		"12345": "",
	} {
		if got := guessLanguage(in); got != want {
			t.Errorf("guessLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWavInfo(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.wav")
	// 0.5 s of 48 kHz mono 16-bit with a LIST chunk before the data
	data := make([]byte, 48000)
	var b []byte
	le := binary.LittleEndian
	b = append(b, "RIFF"...)
	b = le.AppendUint32(b, uint32(4+8+16+8+4+8+len(data)))
	b = append(b, "WAVE"...)
	b = append(b, "fmt "...)
	b = le.AppendUint32(b, 16)
	b = le.AppendUint16(b, 1)
	b = le.AppendUint16(b, 1)
	b = le.AppendUint32(b, 48000)
	b = le.AppendUint32(b, 96000)
	b = le.AppendUint16(b, 2)
	b = le.AppendUint16(b, 16)
	b = append(b, "LIST"...)
	b = le.AppendUint32(b, 4)
	b = append(b, "INFO"...)
	b = append(b, "data"...)
	b = le.AppendUint32(b, uint32(len(data)))
	b = append(b, data...)
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if r, s := wavInfo(f); r != 48000 || s != 0.5 {
		t.Fatalf("wavInfo = %d %v", r, s)
	}
	if r, s := wavInfo(f + ".nope"); r != 0 || s != 0 {
		t.Fatal("missing file")
	}
}

// voicedEndsCheck runs speech.py's voiced_ends (taken out of the embedded worker, the rest of it needs mlx) on tone
// bursts in silence, the words' ends set too late, once loud and once 40 dB quieter; it prints the words as JSON.
const voicedEndsCheck = `
import ast, json, sys
import numpy as np
fn = next(n for n in ast.parse(sys.stdin.read()).body if isinstance(n, ast.FunctionDef) and n.name == "voiced_ends")
exec(compile(ast.Module(body=[fn], type_ignores=[]), "speech.py", "exec"))
sr = 24000
out = []
for amp in (0.5, 0.005):
    t = np.arange(int(3.0 * sr)) / sr
    wav = np.random.default_rng(1).normal(0, amp * 1e-4, t.size)  # a noise floor 80 dB down
    for a, b in ((0.2, 0.6), (0.8, 1.0), (1.0, 1.3), (1.5, 1.9), (2.4, 2.41)):
        on = (t >= a) & (t < b)
        wav[on] += amp * np.sin(2 * np.pi * 220 * t[on])
    words = [
        {"text": "late", "start": 0.2, "end": 0.75},   # voiced to 0.6
        {"text": "tight", "start": 0.8, "end": 0.95},  # voiced past its end: kept
        {"text": "next", "start": 1.0, "end": 1.45},   # voiced to 1.3
        {"text": "long", "start": 1.5, "end": 2.3},    # voiced to 1.9
        {"text": "silent", "start": 2.0, "end": 2.3},  # nothing voiced: kept
        {"text": "short", "start": 2.4, "end": 2.7},   # 10 ms voiced: start + min_len
    ]
    out.append(voiced_ends(words, wav.astype(np.float32), sr))
print(json.dumps(out))
`

func TestVoicedEnds(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	if exec.Command(py, "-c", "import numpy").Run() != nil {
		t.Skip("python3 has no numpy")
	}
	src, err := workerFiles.ReadFile("workers/speech.py")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-c", voicedEndsCheck)
	cmd.Stdin = bytes.NewReader(src)
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	var got [][]Word
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	want := []Word{
		{Text: "late", Start: 0.2, End: 0.63}, {Text: "tight", Start: 0.8, End: 0.95}, {Text: "next", Start: 1.0, End: 1.33},
		{Text: "long", Start: 1.5, End: 1.93}, {Text: "silent", Start: 2.0, End: 2.3}, {Text: "short", Start: 2.4, End: 2.45},
	}
	if len(got) != 2 {
		t.Fatalf("%s", b)
	}
	for i, words := range got {
		if len(words) != len(want) {
			t.Fatalf("clip %d: %v", i, words)
		}
		for j, w := range words {
			if w.Text != want[j].Text || w.Start != want[j].Start || math.Abs(w.End-want[j].End) > 0.011 {
				t.Errorf("clip %d: %+v, want %+v", i, w, want[j])
			}
		}
	}
}

// alignCheck runs speech.py's aligner helpers (taken out of the embedded worker, the rest of it needs mlx): the
// language names, the text's items as written, and the aligner's items merged into the clip's words; it prints JSON.
const alignCheck = `
import ast, json, re, sys, unicodedata
names = {"align_language", "align_text", "_kept", "_cjk", "align_units", "aligned_words"}
body = [n for n in ast.parse(sys.stdin.read()).body if isinstance(n, ast.FunctionDef) and n.name in names
        or isinstance(n, ast.Assign) and any(getattr(t, "id", "") in ("ALIGN_LANGS", "_CONTROL_TAG") for t in n.targets)]
exec(compile(ast.Module(body=body, type_ignores=[]), "speech.py", "exec"))
de = "„Hallo!“ Das ist z.B. ein E-Mail-Test – oder?"
zh = "你好，世界！OK-go 3.5"
items = [("Hallo", 0.0, 0.4), ("Das", 0.5, 0.6), ("ist", 0.6, 0.7), ("zB", 0.7, 1.0), ("ein", 1.0, 1.1),
         ("EMailTest", 1.1, 1.9), ("oder", 2.0, 1.9)]
print(json.dumps({
    "langs": [align_language(x) for x in ("de", "de-DE", "EN", "pt_BR", "zh", "ja", "ko", "hi", "", None)],
    "units": {lang: align_units(t, lang) for lang, t in (("German", de), ("Chinese", zh), ("English", zh))},
    "higgs": align_units("<|emotion:amusement|> Ha, that's funny!", "English"),
    "words": aligned_words(de, "German", items, offset=2.0, limit=3.95),
    "short": aligned_words(de, "German", items[:-1]),
    "other": aligned_words(de, "German", items[:-1] + [("odor", 2.0, 2.1)]),
}))
`

// The aligner's words as the clip's: its languages, the text's words as written (the aligner's own tokenization:
// checked against mlx-audio's ForceAlignProcessor.encode_timestamp on these texts), the items merged.
func TestAlignWords(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	src, err := workerFiles.ReadFile("workers/speech.py")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(py, "-c", alignCheck)
	cmd.Stdin = bytes.NewReader(src)
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	var got struct {
		Langs []*string
		Units map[string][][2]string
		Higgs [][2]string
		Words []Word
		Short []Word
		Other []Word
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	var langs []string
	for _, l := range got.Langs {
		if l == nil {
			langs = append(langs, "-")
		} else {
			langs = append(langs, *l)
		}
	}
	if want := []string{"German", "German", "English", "Portuguese", "Chinese", "-", "-", "-", "-", "-"}; !reflect.DeepEqual(langs, want) {
		t.Errorf("languages %v, want %v", langs, want)
	}
	// the Go side asks for the same languages
	if len(alignLangs) != 8 || !slices.Contains(alignLangs, "de") || slices.Contains(alignLangs, "ja") {
		t.Errorf("alignLangs %v", alignLangs)
	}
	// words as written without the marks around them (inner ones stay), "–" none; Chinese: a character each, and a
	// word ends at any mark
	for lang, want := range map[string][][2]string{
		"German": {{"Hallo", "Hallo"}, {"Das", "Das"}, {"ist", "ist"}, {"z.B", "zB"}, {"ein", "ein"},
			{"E-Mail-Test", "EMailTest"}, {"oder", "oder"}},
		"Chinese": {{"你", "你"}, {"好", "好"}, {"世", "世"}, {"界", "界"}, {"OK", "OK"}, {"go", "go"}, {"3", "3"}, {"5", "5"}},
		"English": {{"你", "你"}, {"好", "好"}, {"世", "世"}, {"界", "界"}, {"OK-go", "OKgo"}, {"3.5", "35"}},
	} {
		if !reflect.DeepEqual(got.Units[lang], want) {
			t.Errorf("%s: %q, want %q", lang, got.Units[lang], want)
		}
	}
	if want := [][2]string{{"Ha", "Ha"}, {"that's", "that's"}, {"funny", "funny"}}; !reflect.DeepEqual(got.Higgs, want) {
		t.Errorf("higgs tags: %q", got.Higgs)
	}
	// moved by the sentence's start, kept before its end, never ending before they start
	want := []Word{{"Hallo", 2, 2.4}, {"Das", 2.5, 2.6}, {"ist", 2.6, 2.7}, {"z.B", 2.7, 3}, {"ein", 3, 3.1},
		{"E-Mail-Test", 3.1, 3.9}, {"oder", 3.95, 3.95}}
	if !reflect.DeepEqual(got.Words, want) {
		t.Errorf("words %v\nwant %v", got.Words, want)
	}
	if got.Short != nil || got.Other != nil {
		t.Errorf("items that do not match the text: %v %v", got.Short, got.Other)
	}
}
