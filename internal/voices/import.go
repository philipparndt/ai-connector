package voices

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/philipparndt/ai-connector/internal/engine"
)

// A voice set is a folder of reference clips with their transcripts, described by a voices.yaml (as tuned by
// listening for Fish S2 Pro):
//
//	dir: fish-s2-pro          # the clips' folder, beside the file (default: the file's own)
//	voices:
//	  de-male:
//	    language: German      # a name or a code (de)
//	    reference: de-male.wav
//	    transcript: "In diesem Tutorial zeichnen wir …"
//
// Other keys (model, gender, rating, origin, …) are left as they are. Each voice becomes a cloned voice of the
// library named as in the set; its clip keeps its sample rate (S2 Pro's codec is 44.1 kHz).
type voiceSet struct {
	Dir    string    `yaml:"dir"`
	Voices yaml.Node `yaml:"voices"`
}

type setVoice struct {
	Language   string `yaml:"language"`
	Reference  string `yaml:"reference"`
	Transcript string `yaml:"transcript"`
}

// languageCodes: a set's language names as the library's codes.
var languageCodes = map[string]string{"english": "en", "german": "de", "deutsch": "de", "french": "fr", "spanish": "es",
	"italian": "it", "portuguese": "pt", "dutch": "nl", "polish": "pl", "russian": "ru", "chinese": "zh",
	"japanese": "ja", "korean": "ko", "arabic": "ar", "turkish": "tr", "swedish": "sv", "danish": "da",
	"norwegian": "no", "finnish": "fi", "greek": "el", "hindi": "hi"}

func languageCode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if c, ok := languageCodes[s]; ok {
		return c
	}
	return s
}

// Imported is what ImportSet did: the voices it added, and those it left out (by name, with the reason).
type Imported struct {
	Added   []engine.Voice
	Skipped map[string]string
}

// ImportSet adds the voices of a voice set (its voices.yaml). A voice whose name the library has already is left
// out (importing twice changes nothing); the set's files are only read.
func (l *Library) ImportSet(ctx context.Context, file string) (Imported, error) {
	res := Imported{Skipped: map[string]string{}}
	b, err := os.ReadFile(file)
	if err != nil {
		return res, err
	}
	var set voiceSet
	if err := yaml.Unmarshal(b, &set); err != nil {
		return res, fmt.Errorf("%s: %w", file, err)
	}
	if set.Voices.Kind != yaml.MappingNode || len(set.Voices.Content) == 0 {
		return res, fmt.Errorf("%s: no voices", file)
	}
	dir := filepath.Dir(file)
	if set.Dir != "" {
		if filepath.IsAbs(set.Dir) {
			dir = set.Dir
		} else {
			dir = filepath.Join(dir, set.Dir)
		}
	}
	for i := 0; i+1 < len(set.Voices.Content); i += 2 {
		name := set.Voices.Content[i].Value
		var v setVoice
		if err := set.Voices.Content[i+1].Decode(&v); err != nil {
			res.Skipped[name] = err.Error()
			continue
		}
		if _, ok := l.Voice(name); ok {
			res.Skipped[name] = "the library has a voice of that name"
			continue
		}
		if v.Reference == "" || strings.TrimSpace(v.Transcript) == "" {
			res.Skipped[name] = "a reference clip and its transcript are needed"
			continue
		}
		ref := v.Reference
		if !filepath.IsAbs(ref) {
			ref = filepath.Join(dir, ref)
		}
		f, err := os.Open(ref)
		if err != nil {
			res.Skipped[name] = err.Error()
			continue
		}
		voice, err := l.addClone(ctx, name, f, v.Transcript, languageCode(v.Language), 0)
		f.Close()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return res, err
			}
			res.Skipped[name] = err.Error()
			continue
		}
		res.Added = append(res.Added, voice)
	}
	return res, nil
}
