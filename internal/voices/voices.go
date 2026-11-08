// Package voices is the voice library of the speech models (docs/voice-avatar.md): <config dir>/voices/voices.json
// and the samples next to it. A cloned voice is a short sample of a speaker (converted to 24 kHz mono, at most 20 s)
// with its transcript; a designed voice is a description. The file is read on every call, so every Library on the
// same folder (the speech backend's, the web server's) sees the same voices.
package voices

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// MaxSample is the longest sample kept (seconds); the models want 5–15 s.
const MaxSample = 20

// SampleRate of the stored samples.
const SampleRate = 24000

// ErrNotFound: no voice of that id.
var ErrNotFound = errors.New("no such voice")

// mu guards every voices.json of this process.
var mu sync.Mutex

// Library is the voices of one folder.
type Library struct {
	Dir string
	// Transcribe writes down what a sample says (the speech backend's Qwen3-ASR); nil: a sample without a
	// transcript keeps none.
	Transcribe func(ctx context.Context, file, language string) (string, error)
	// Preview says a short text with a designed voice into out (the speech backend); nil: designed voices have no
	// sample.
	Preview func(ctx context.Context, v engine.Voice, out string) error
}

// Open is the library in dir (made when the first voice is added).
func Open(dir string) *Library { return &Library{Dir: dir} }

func (l *Library) file() string { return filepath.Join(l.Dir, "voices.json") }

func (l *Library) load() ([]engine.Voice, error) {
	b, err := os.ReadFile(l.file())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var vs []engine.Voice
	if err := json.Unmarshal(b, &vs); err != nil {
		return nil, fmt.Errorf("%s: %w", l.file(), err)
	}
	return vs, nil
}

func (l *Library) save(vs []engine.Voice) error {
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return err
	}
	if vs == nil {
		vs = []engine.Voice{}
	}
	b, err := json.MarshalIndent(vs, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.file() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.file())
}

// List is every voice, oldest first.
func (l *Library) List() ([]engine.Voice, error) {
	mu.Lock()
	defer mu.Unlock()
	return l.load()
}

// Voice finds a voice by its id, else by its name (case does not matter). It is the speech backend's VoiceStore.
func (l *Library) Voice(id string) (engine.Voice, bool) {
	vs, err := l.List()
	if err != nil {
		return engine.Voice{}, false
	}
	for _, v := range vs {
		if v.ID == id {
			return v, true
		}
	}
	for _, v := range vs {
		if strings.EqualFold(v.Name, strings.TrimSpace(id)) {
			return v, true
		}
	}
	return engine.Voice{}, false
}

// newID is a readable, unique id: the name's slug and a random suffix.
func newID(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == 'ä':
			b.WriteString("ae")
		case r == 'ö':
			b.WriteString("oe")
		case r == 'ü':
			b.WriteString("ue")
		case r == 'ß':
			b.WriteString("ss")
		case unicode.IsSpace(r) || r == '-' || r == '_':
			if s := b.String(); s != "" && !strings.HasSuffix(s, "-") {
				b.WriteByte('-')
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 24 {
		slug = strings.Trim(slug[:24], "-")
	}
	if slug == "" {
		slug = "voice"
	}
	var rb [3]byte
	_, _ = rand.Read(rb[:])
	return slug + "-" + hex.EncodeToString(rb[:])
}

func cleanName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return "", errors.New("a name for the voice")
	}
	if len([]rune(name)) > 80 {
		return "", errors.New("the name is too long (80 characters at most)")
	}
	return name, nil
}

// AddClone adds a voice from a sample of a speaker (any format ffmpeg reads): converted to 24 kHz mono wav and cut
// to MaxSample seconds. Without a transcript, Transcribe writes it.
func (l *Library) AddClone(ctx context.Context, name string, sample io.Reader, transcript, language string) (engine.Voice, error) {
	return l.addClone(ctx, name, sample, transcript, language, SampleRate)
}

// addClone adds a cloned voice with its sample at rate (0: the sample's own, e.g. a set's 44.1 kHz references).
func (l *Library) addClone(ctx context.Context, name string, sample io.Reader, transcript, language string, rate int) (engine.Voice, error) {
	name, err := cleanName(name)
	if err != nil {
		return engine.Voice{}, err
	}
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return engine.Voice{}, err
	}
	id := newID(name)
	raw, err := os.CreateTemp(l.Dir, ".upload-*")
	if err != nil {
		return engine.Voice{}, err
	}
	defer os.Remove(raw.Name())
	if _, err := io.Copy(raw, io.LimitReader(sample, 64<<20)); err != nil {
		raw.Close()
		return engine.Voice{}, err
	}
	raw.Close()
	out := filepath.Join(l.Dir, id+".wav")
	if err := convert(ctx, raw.Name(), out, rate); err != nil {
		return engine.Voice{}, err
	}
	if d := seconds(out); d < 1 {
		os.Remove(out)
		return engine.Voice{}, fmt.Errorf("the sample is too short (%.1f s; 5–15 s of clear speech work best)", d)
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" && l.Transcribe != nil {
		t, err := l.Transcribe(ctx, out, language)
		if err != nil {
			os.Remove(out)
			return engine.Voice{}, fmt.Errorf("transcribing the sample: %w", err)
		}
		transcript = strings.TrimSpace(t)
	}
	v := engine.Voice{ID: id, Name: name, Kind: "clone", Sample: out, Transcript: transcript,
		Language: strings.ToLower(strings.TrimSpace(language)), Created: time.Now().UTC().Format(time.RFC3339)}
	mu.Lock()
	defer mu.Unlock()
	vs, err := l.load()
	if err == nil {
		err = l.save(append(vs, v))
	}
	if err != nil {
		os.Remove(out)
		return engine.Voice{}, err
	}
	return v, nil
}

// AddDesign adds a voice described in words ("a calm older woman, low voice").
// With Preview set, it also gets a short sample of how it sounds.
func (l *Library) AddDesign(ctx context.Context, name, description, language string) (engine.Voice, error) {
	name, err := cleanName(name)
	if err != nil {
		return engine.Voice{}, err
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return engine.Voice{}, errors.New("a description of the voice")
	}
	v := engine.Voice{ID: newID(name), Name: name, Kind: "design", Description: description,
		Language: strings.ToLower(strings.TrimSpace(language)), Created: time.Now().UTC().Format(time.RFC3339)}
	if err := l.change(func(vs []engine.Voice) ([]engine.Voice, error) { return append(vs, v), nil }); err != nil {
		return engine.Voice{}, err
	}
	return l.preview(ctx, v), nil
}

// change edits the list under the lock and saves it.
func (l *Library) change(f func([]engine.Voice) ([]engine.Voice, error)) error {
	mu.Lock()
	defer mu.Unlock()
	vs, err := l.load()
	if err != nil {
		return err
	}
	if vs, err = f(vs); err != nil {
		return err
	}
	return l.save(vs)
}

// preview renders a designed voice's sample (with Preview; a failure leaves the voice without one). It runs
// outside the lock: the speech backend looks the voice up in the library.
func (l *Library) preview(ctx context.Context, v engine.Voice) engine.Voice {
	if l.Preview == nil || v.Kind != "design" {
		return v
	}
	out := filepath.Join(l.Dir, v.ID+".wav")
	tmp := filepath.Join(l.Dir, "."+v.ID+".preview.wav")
	if err := l.Preview(ctx, v, tmp); err != nil {
		os.Remove(tmp)
		return v
	}
	if err := os.Rename(tmp, out); err != nil {
		return v
	}
	_ = l.change(func(vs []engine.Voice) ([]engine.Voice, error) {
		for i := range vs {
			if vs[i].ID == v.ID {
				vs[i].Sample = out
			}
		}
		return vs, nil
	})
	v.Sample = out
	return v
}

// Patch changes a voice (nil: unchanged).
type Patch struct {
	Name        *string `json:"name"`
	Transcript  *string `json:"transcript"`
	Description *string `json:"description"`
	Language    *string `json:"language"`
}

// Update changes a voice (a designed voice with a new description gets a new preview).
func (l *Library) Update(ctx context.Context, id string, p Patch) (engine.Voice, error) {
	v, err := l.update(id, p)
	if err == nil && p.Description != nil {
		v = l.preview(ctx, v)
	}
	return v, err
}

func (l *Library) update(id string, p Patch) (engine.Voice, error) {
	mu.Lock()
	defer mu.Unlock()
	vs, err := l.load()
	if err != nil {
		return engine.Voice{}, err
	}
	i := slices.IndexFunc(vs, func(v engine.Voice) bool { return v.ID == id })
	if i < 0 {
		return engine.Voice{}, ErrNotFound
	}
	v := vs[i]
	if p.Name != nil {
		if v.Name, err = cleanName(*p.Name); err != nil {
			return engine.Voice{}, err
		}
	}
	if p.Transcript != nil {
		if v.Kind != "clone" {
			return engine.Voice{}, errors.New("only a cloned voice has a transcript")
		}
		v.Transcript = strings.TrimSpace(*p.Transcript)
	}
	if p.Description != nil {
		if v.Kind != "design" {
			return engine.Voice{}, errors.New("only a designed voice has a description")
		}
		if v.Description = strings.TrimSpace(*p.Description); v.Description == "" {
			return engine.Voice{}, errors.New("a description of the voice")
		}
	}
	if p.Language != nil {
		v.Language = strings.ToLower(strings.TrimSpace(*p.Language))
	}
	vs[i] = v
	return v, l.save(vs)
}

// Delete removes a voice and its sample.
func (l *Library) Delete(id string) error {
	mu.Lock()
	defer mu.Unlock()
	vs, err := l.load()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(vs, func(v engine.Voice) bool { return v.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	if s := vs[i].Sample; s != "" && filepath.Dir(s) == filepath.Clean(l.Dir) {
		_ = os.Remove(s)
	}
	return l.save(slices.Delete(vs, i, i+1))
}

// Convert makes a sample of any audio file: 24 kHz mono 16-bit wav, leading silence cut, at most MaxSample seconds.
func Convert(ctx context.Context, in, out string) error { return convert(ctx, in, out, SampleRate) }

// convert is Convert at another sample rate (0: the file's own).
func convert(ctx context.Context, in, out string, rate int) error {
	args := []string{"-y", "-v", "error", "-i", in, "-vn",
		"-af", "silenceremove=start_periods=1:start_threshold=-50dB:start_silence=0.1", "-ac", "1"}
	if rate > 0 {
		args = append(args, "-ar", fmt.Sprint(rate))
	}
	args = append(args, "-t", fmt.Sprint(MaxSample), "-c:a", "pcm_s16le", "-map_metadata", "-1", "-bitexact", out)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if b, err := cmd.CombinedOutput(); err != nil {
		os.Remove(out)
		if errors.Is(err, exec.ErrNotFound) {
			return errors.New("ffmpeg is needed for voice samples (brew install ffmpeg)")
		}
		return fmt.Errorf("not an audio file ffmpeg can read: %s", strings.TrimSpace(string(b)))
	}
	return nil
}

// seconds is the length of a 16-bit mono wav as convert writes it (a 44-byte header; its byte rate at 28).
func seconds(file string) float64 {
	b, err := os.ReadFile(file)
	if err != nil || len(b) < 44 {
		return 0
	}
	byteRate := binary.LittleEndian.Uint32(b[28:32])
	if byteRate == 0 {
		return 0
	}
	return float64(len(b)-44) / float64(byteRate)
}
