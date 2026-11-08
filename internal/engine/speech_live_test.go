package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/philipparndt/ai-connector/internal/models"
)

// TestSpeechLive runs the speech models for real (downloads, the mlx-audio runtime, the GPU): MLXLAB_SPEECH_LIVE=1,
// MLXLAB_SPEECH_MODELS=qwen3-tts-1.7b,voxtral-tts (default: all). It speaks one German sentence with a preset (or
// the model's own voice), designs a voice with Qwen3-TTS 1.7B VoiceDesign for a sample, clones it with every model
// that clones, and transcribes every output with Qwen3-ASR (word error rate against the text).
func TestSpeechLive(t *testing.T) {
	if os.Getenv("MLXLAB_SPEECH_LIVE") == "" {
		t.Skip("MLXLAB_SPEECH_LIVE=1 runs the speech models")
	}
	root := ConfigDir()
	srcs, _ := models.Sources(os.Getenv("MLXLAB_MODELS_FROM"), nil)
	store := &models.Store{Dir: filepath.Join(root, "models"), Sources: srcs}
	out := os.Getenv("MLXLAB_SPEECH_OUT")
	if out == "" {
		out = t.TempDir()
	}
	lib := fakeVoices{}
	b := &SpeechBackend{Root: root, Store: store, Voices: lib}
	defer b.Unload()
	ctx := context.Background()
	emit := func(ev Event) {
		if ev.Type == "status" || ev.Type == "log" {
			t.Log(ev.Text)
		}
	}
	const text = "Guten Abend! Das ist ein Test der lokalen Sprachausgabe."
	byID := map[string]*Model{}
	for _, m := range speechModels() {
		byID[m.ID] = m
	}
	want := strings.Split(os.Getenv("MLXLAB_SPEECH_MODELS"), ",")
	if want[0] == "" {
		want = []string{"qwen3-tts-1.7b", "qwen3-tts-0.6b", "chatterbox-v3", "voxcpm2", "higgs-audio-v3", "voxtral-tts", "fish-s2-pro"}
	}
	say := func(id, voice, name string) {
		m := byID[id]
		var stats map[string]any = map[string]any{}
		file := filepath.Join(out, id+"-"+name+".wav")
		_, err := b.speak(ctx, m, Request{Text: text, Voice: voice, Language: "de", Seed: 7}, file, func(ev Event) {
			if ev.Type == "stats" {
				for k, v := range ev.Data {
					stats[k] = v
				}
			}
			emit(ev)
		})
		if err != nil {
			t.Errorf("%s %s: %v", id, name, err)
			return
		}
		heard, err := b.transcribe(ctx, file, "de", emit)
		if err != nil {
			t.Errorf("%s %s: transcribe: %v", id, name, err)
		}
		t.Logf("RESULT %s %s: %.1f s audio in %.1f s, rtf %.2f, peak %.1f GB, %v Hz, WER %.2f — %q", id, name,
			stats["duration"], stats["seconds"], stats["rtf"], stats["peakGB"], stats["sampleRate"], wer(text, heard), heard)
	}

	// the sample: a voice designed by Qwen3-TTS 1.7B (no real person's voice)
	sample := filepath.Join(out, "sample-designed.wav")
	lib["designed"] = Voice{ID: "designed", Name: "Designed", Kind: "design", Language: "de",
		Description: "A man in his forties with a warm, calm baritone voice, speaking clear standard German at a relaxed pace."}
	if _, err := os.Stat(sample); err != nil {
		_, err := b.speak(ctx, byID["qwen3-tts-1.7b"], Request{Voice: "designed", Language: "de", Seed: 11,
			Text: "Ich wohne seit vielen Jahren in einer kleinen Stadt am Fluss. Morgens trinke ich gern einen Kaffee und lese die Zeitung, bevor die Arbeit beginnt."},
			sample, emit)
		if err != nil {
			t.Fatalf("designing the sample: %v", err)
		}
	}
	transcript, err := b.transcribe(ctx, sample, "de", emit)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sample: %.1f s, transcript %q", wavSeconds(sample), transcript)
	lib["clone"] = Voice{ID: "clone", Name: "Clone", Kind: "clone", Sample: sample, Transcript: transcript, Language: "de"}

	if os.Getenv("MLXLAB_SPEECH_LONG") != "" { // sentences, paragraphs, instruct and speed (time-stretched)
		long := "Es war einmal ein kleiner Fuchs, der in einem Wald voller hoher Tannen lebte. Jeden Abend saß er auf einem Hügel und zählte die Sterne.\n\n" +
			"Eines Nachts fiel ein Stern direkt vor seine Pfoten. „Wer bist du?“, fragte der Fuchs leise."
		file := filepath.Join(out, "long.wav")
		var events []string
		_, err := b.speak(ctx, byID["qwen3-tts-1.7b"], Request{Text: long, Voice: "Vivian", Instruct: "warm, slow, like a bedtime story",
			Speed: 1.15, Seed: 3}, file, func(ev Event) {
			if ev.Type == "progress" || ev.Type == "stats" {
				events = append(events, fmt.Sprint(ev.Step, "/", ev.Total, ev.Data))
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		heard, _ := b.transcribe(ctx, file, "de", emit)
		t.Logf("RESULT long: %v, WER %.2f — %q", events, wer(long, heard), heard)
	}
	for _, id := range want {
		if byID[id] == nil {
			continue // MLXLAB_SPEECH_MODELS=none: only the sample (and the long text)
		}
		fam := speechFamilies[byID[id].workerModel]
		say(id, "", "preset")
		if fam.clone {
			say(id, "clone", "clone")
		}
		if id == "qwen3-tts-1.7b" || id == "voxcpm2" {
			say(id, "designed", "design")
		}
	}
}

// wer is the word error rate of heard against said (case and punctuation ignored).
func wer(said, heard string) float64 {
	words := func(s string) []string {
		return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	}
	a, b := words(said), words(heard)
	if len(a) == 0 {
		return 0
	}
	d := make([]int, len(b)+1)
	for j := range d {
		d[j] = j
	}
	for i := 1; i <= len(a); i++ {
		prev := d[0]
		d[0] = i
		for j := 1; j <= len(b); j++ {
			cur := d[j]
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[j] = min(d[j]+1, d[j-1]+1, prev+cost)
			prev = cur
		}
	}
	return float64(d[len(b)]) / float64(len(a))
}

func TestWER(t *testing.T) {
	if w := wer("Guten Abend! Das ist ein Test.", "guten abend das ist ein test"); w != 0 {
		t.Fatal(w)
	}
	if w := wer("a b c d", "a x c"); fmt.Sprintf("%.2f", w) != "0.50" {
		t.Fatal(w)
	}
}
