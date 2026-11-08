package voices

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// tone writes a sine tone of the given length (mp3, stereo 44.1 kHz: Convert has work to do).
func tone(t *testing.T, seconds string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	f := filepath.Join(t.TempDir(), "tone.mp3")
	out, err := exec.Command("ffmpeg", "-y", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration="+seconds,
		"-ac", "2", "-ar", "44100", f).CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	return f
}

func probe(t *testing.T, f string) string {
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=sample_rate,channels,codec_name:format=duration",
		"-of", "default=noprint_wrappers=1", f).Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestLibrary(t *testing.T) {
	ctx := context.Background()
	lib := Open(filepath.Join(t.TempDir(), "voices"))
	if vs, err := lib.List(); err != nil || len(vs) != 0 {
		t.Fatalf("empty library: %v %v", vs, err)
	}

	// a clone with a transcript: converted, cut to 20 s
	src, _ := os.Open(tone(t, "25"))
	defer src.Close()
	anna, err := lib.AddClone(ctx, "  Anna  Müller ", src, " Hallo, ich bin Anna. ", "DE")
	if err != nil {
		t.Fatal(err)
	}
	if anna.Name != "Anna Müller" || anna.Kind != "clone" || anna.Transcript != "Hallo, ich bin Anna." || anna.Language != "de" ||
		!strings.HasPrefix(anna.ID, "anna-mueller-") || anna.Created == "" {
		t.Fatalf("voice %+v", anna)
	}
	if filepath.Dir(anna.Sample) != lib.Dir {
		t.Fatalf("sample %s", anna.Sample)
	}
	p := probe(t, anna.Sample)
	for _, want := range []string{"codec_name=pcm_s16le", "sample_rate=24000", "channels=1", "duration=20.0"} {
		if !strings.Contains(p, want) {
			t.Errorf("sample: want %s in\n%s", want, p)
		}
	}
	if d := seconds(anna.Sample); d < 19.9 || d > 20.01 {
		t.Errorf("seconds %v", d)
	}

	// without a transcript: Transcribe writes it (with the language)
	var asked string
	lib.Transcribe = func(_ context.Context, file, language string) (string, error) {
		asked = language
		if _, err := os.Stat(file); err != nil {
			t.Errorf("transcribe a missing file: %v", err)
		}
		return " Guten Tag. ", nil
	}
	src2, _ := os.Open(tone(t, "6"))
	defer src2.Close()
	bert, err := lib.AddClone(ctx, "Bert", src2, "", "de")
	if err != nil || bert.Transcript != "Guten Tag." || asked != "de" {
		t.Fatalf("transcribed %+v %v (%q)", bert, err, asked)
	}
	// a failing transcription leaves nothing behind
	lib.Transcribe = func(context.Context, string, string) (string, error) { return "", errors.New("no model") }
	src3, _ := os.Open(tone(t, "6"))
	defer src3.Close()
	if _, err := lib.AddClone(ctx, "Carl", src3, "", ""); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("transcription error: %v", err)
	}
	// too short, not audio, no name
	src4, _ := os.Open(tone(t, "0.5"))
	defer src4.Close()
	if _, err := lib.AddClone(ctx, "Kurz", src4, "x", ""); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("short sample: %v", err)
	}
	if _, err := lib.AddClone(ctx, "Text", strings.NewReader("not audio at all"), "x", ""); err == nil {
		t.Fatal("not audio accepted")
	}
	if _, err := lib.AddClone(ctx, " ", strings.NewReader(""), "x", ""); err == nil {
		t.Fatal("no name accepted")
	}
	if wavs, _ := filepath.Glob(filepath.Join(lib.Dir, "*.wav")); len(wavs) != 2 {
		t.Fatalf("samples left behind: %v", wavs)
	}

	// a designed voice
	oma, err := lib.AddDesign(ctx, "Oma", "a calm older woman, low voice", "de")
	if err != nil || oma.Kind != "design" || oma.Sample != "" {
		t.Fatalf("design %+v %v", oma, err)
	}
	if _, err := lib.AddDesign(ctx, "Leer", " ", ""); err == nil {
		t.Fatal("a design without a description")
	}

	// look up by id and by name; another Library on the folder sees the same
	other := Open(lib.Dir)
	if v, ok := other.Voice(anna.ID); !ok || v.Name != "Anna Müller" {
		t.Fatal("by id")
	}
	if v, ok := other.Voice("oma"); !ok || v.ID != oma.ID {
		t.Fatal("by name")
	}
	if _, ok := other.Voice("nobody"); ok {
		t.Fatal("unknown voice found")
	}

	// edit
	name, tr, desc := "Anna M.", "Hallo!", "a cheerful grandmother"
	if v, err := lib.Update(ctx, anna.ID, Patch{Name: &name, Transcript: &tr}); err != nil || v.Name != name || v.Transcript != tr || v.Sample != anna.Sample {
		t.Fatalf("update %+v %v", v, err)
	}
	if _, err := lib.Update(ctx, anna.ID, Patch{Description: &desc}); err == nil {
		t.Fatal("a description for a clone")
	}
	if _, err := lib.Update(ctx, oma.ID, Patch{Transcript: &tr}); err == nil {
		t.Fatal("a transcript for a design")
	}
	if v, err := lib.Update(ctx, oma.ID, Patch{Description: &desc}); err != nil || v.Description != desc {
		t.Fatalf("design update %+v %v", v, err)
	}
	if _, err := lib.Update(ctx, "nope", Patch{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update unknown: %v", err)
	}

	// delete takes the sample along
	if err := lib.Delete(anna.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(anna.Sample); !os.IsNotExist(err) {
		t.Fatal("sample kept")
	}
	if err := lib.Delete(anna.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	vs, _ := lib.List()
	if len(vs) != 2 || vs[0].ID != bert.ID || vs[1].ID != oma.ID {
		t.Fatalf("left: %+v", vs)
	}
}

// The browser records webm/opus (Chrome) or mp4/aac (Safari); a designed voice gets a preview from Preview.
func TestBrowserFormatsAndPreview(t *testing.T) {
	ctx := context.Background()
	src := tone(t, "6")
	lib := Open(filepath.Join(t.TempDir(), "voices"))
	for _, f := range []struct{ ext, codec string }{{"webm", "libopus"}, {"mp4", "aac"}} {
		in := filepath.Join(t.TempDir(), "rec."+f.ext)
		if out, err := exec.Command("ffmpeg", "-y", "-v", "error", "-i", src, "-c:a", f.codec, in).CombinedOutput(); err != nil {
			t.Skipf("ffmpeg cannot write %s: %v %s", f.codec, err, out)
		}
		r, _ := os.Open(in)
		v, err := lib.AddClone(ctx, "Rec "+f.ext, r, "Hallo.", "de")
		r.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.ext, err)
		}
		if p := probe(t, v.Sample); !strings.Contains(p, "sample_rate=24000") || !strings.Contains(p, "channels=1") {
			t.Fatalf("%s: %s", f.ext, p)
		}
	}

	var said engine.Voice
	lib.Preview = func(_ context.Context, v engine.Voice, out string) error {
		said = v
		if v2, ok := lib.Voice(v.ID); !ok || v2.Description != v.Description { // saved before the preview
			t.Error("preview of an unsaved voice")
		}
		return exec.Command("ffmpeg", "-y", "-v", "error", "-i", src, out).Run()
	}
	oma, err := lib.AddDesign(ctx, "Oma", "a calm older woman", "de")
	if err != nil || said.ID != oma.ID || oma.Sample != filepath.Join(lib.Dir, oma.ID+".wav") {
		t.Fatalf("preview %+v %v", oma, err)
	}
	if v, _ := lib.Voice(oma.ID); v.Sample != oma.Sample {
		t.Fatal("sample not saved")
	}
	// a failing preview keeps the voice (without a new sample)
	lib.Preview = func(context.Context, engine.Voice, string) error { return errors.New("no model") }
	if v, err := lib.AddDesign(ctx, "Opa", "a deep old voice", "de"); err != nil || v.Sample != "" {
		t.Fatalf("failed preview: %+v %v", v, err)
	}
	if err := lib.Delete(oma.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oma.Sample); !os.IsNotExist(err) {
		t.Fatal("preview kept after delete")
	}
}

func TestNewID(t *testing.T) {
	for in, prefix := range map[string]string{"Anna Müller": "anna-mueller-", "  ": "voice-", "Groß & Klein!": "gross-klein-",
		"A very long name that goes on and on": "a-very-long-name-that-go-"} {
		if id := newID(in); !strings.HasPrefix(id, prefix) || len(id) != len(prefix)+6 {
			t.Errorf("newID(%q) = %q, want %s…", in, id, prefix)
		}
	}
}

// A voice set (voices.yaml with its clips) becomes cloned voices named as in the set, the clips at their own rate;
// a voice without a transcript or clip is left out, and a second import adds nothing.
func TestImportSet(t *testing.T) {
	ctx := context.Background()
	set := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(set, "fish-s2-pro"), 0o755))
	b, err := os.ReadFile(tone(t, "6"))
	must(err)
	must(os.WriteFile(filepath.Join(set, "fish-s2-pro", "de-male.mp3"), b, 0o644))
	must(os.WriteFile(filepath.Join(set, "fish-s2-pro", "en-gb-female.mp3"), b, 0o644))
	file := filepath.Join(set, "voices.yaml")
	must(os.WriteFile(file, []byte(`# a set
model: fish-s2-pro
dir: fish-s2-pro
voices:
  en-gb-female:
    language: English
    accent: British
    reference: en-gb-female.mp3
    transcript: "In this tutorial, we'll draw a sketch."
    origin: {seed: 3, language: null, tag: null}
  de-male:
    language: German
    reference: de-male.mp3
    transcript: "In diesem Tutorial zeichnen wir eine Skizze."
  no-text:
    language: German
    reference: de-male.mp3
  gone:
    language: de
    reference: gone.wav
    transcript: "Hallo."
`), 0o644))
	lib := Open(filepath.Join(t.TempDir(), "voices"))
	res, err := lib.ImportSet(ctx, file)
	must(err)
	if len(res.Added) != 2 || res.Added[0].Name != "en-gb-female" || res.Added[1].Name != "de-male" || len(res.Skipped) != 2 ||
		res.Skipped["no-text"] == "" || res.Skipped["gone"] == "" {
		t.Fatalf("import %+v", res)
	}
	de, ok := lib.Voice("de-male")
	if !ok || de.Kind != "clone" || de.Language != "de" || de.Transcript != "In diesem Tutorial zeichnen wir eine Skizze." ||
		filepath.Dir(de.Sample) != lib.Dir {
		t.Fatalf("de-male %+v", de)
	}
	if en, _ := lib.Voice("en-gb-female"); en.Language != "en" {
		t.Fatalf("en-gb-female %+v", en)
	}
	if p := probe(t, de.Sample); !strings.Contains(p, "sample_rate=44100") || !strings.Contains(p, "channels=1") {
		t.Errorf("the clip's own rate, mono: %s", p)
	}
	if d := seconds(de.Sample); d < 5.5 || d > 6.01 {
		t.Errorf("seconds %v", d)
	}
	// again: nothing new
	res, err = lib.ImportSet(ctx, file)
	must(err)
	if vs, _ := lib.List(); len(res.Added) != 0 || len(vs) != 2 {
		t.Fatalf("second import %+v, %d voices", res, len(vs))
	}
	if _, err := lib.ImportSet(ctx, filepath.Join(set, "missing.yaml")); err == nil {
		t.Fatal("a missing set")
	}
}
