package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/llm"
)

// runOptions are the flags of "mlxlab run".
type runOptions struct {
	size                          string
	steps, frames, fps, quantize  int
	seed                          int64
	image                         string
	temp                          float64
	think, lowRAM, noAudio, quiet bool
	lyrics                        string
	duration                      float64
	instrumental                  bool
	bpm                           int
	planner                       string
	maxTokens                     int
	voice, language, instruct     string
	speed                         float64
}

func (o *runOptions) flags(fs *flag.FlagSet) {
	fs.StringVar(&o.size, "size", "", "image/video size WxH")
	fs.IntVar(&o.steps, "steps", 0, "denoising steps (image; LTX-2.5 HQ: stage 1)")
	fs.Int64Var(&o.seed, "seed", 0, "seed (0: random)")
	fs.IntVar(&o.frames, "frames", 0, "video frames, 8k+1")
	fs.IntVar(&o.fps, "fps", 0, "video frame rate")
	fs.IntVar(&o.quantize, "q", 0, "image model precision: 4, 8, -1 = bf16")
	fs.StringVar(&o.image, "image", "", "reference image (edit) or first frame (video)")
	fs.Float64Var(&o.temp, "temp", 0.3, "chat temperature")
	fs.IntVar(&o.maxTokens, "max-tokens", 2048, "chat: most tokens to write")
	fs.BoolVar(&o.think, "think", false, "chat: let the model think first")
	fs.BoolVar(&o.lowRAM, "low-ram", false, "video: stream the transformer from disk")
	fs.BoolVar(&o.noAudio, "no-audio", false, "video: no audio track")
	fs.BoolVar(&o.quiet, "quiet", false, "no status lines on stderr")
	fs.StringVar(&o.lyrics, "lyrics", "", "music: a file with the lyrics ([verse], [chorus] …)")
	fs.Float64Var(&o.duration, "duration", 0, "music: seconds")
	fs.BoolVar(&o.instrumental, "instrumental", false, "music: no vocals")
	fs.IntVar(&o.bpm, "bpm", 0, "music: the tempo (default: \"NNN bpm\" in the style)")
	fs.StringVar(&o.planner, "planner", "", "music (ACE-Step): the planner LM, 4b or off")
	fs.StringVar(&o.voice, "voice", "", "speech: a preset of the model or a voice of the library (mlxlab voices list)")
	fs.StringVar(&o.language, "language", "", "speech: the text's language (de, en, …; default: as the text looks)")
	fs.StringVar(&o.instruct, "instruct", "", "speech: how to say it (\"calm, slow\"), where the model takes it")
	fs.Float64Var(&o.speed, "speed", 0, "speech: 0.5–2 (default 1)")
}

func (o *runOptions) run(mgr *jobs.Manager, args []string) error {
	defer mgr.Unload() // like mlxlab serve: a server runtime's server, with its model in memory, does not outlive the job
	if len(args) < 2 {
		usage()
		return errors.New("run: a model and a prompt")
	}
	md := mgr.Catalog.Find(args[0])
	if md == nil {
		return fmt.Errorf("no model %q (mlxlab list)", args[0])
	}
	text := strings.Join(args[1:], " ")
	r := engine.Request{Model: md.ID, Steps: o.steps, Seed: o.seed, Frames: o.frames, FPS: o.fps, Quantize: o.quantize,
		LowRAM: o.lowRAM, NoAudio: o.noAudio}
	if o.size != "" {
		if _, err := fmt.Sscanf(o.size, "%dx%d", &r.Width, &r.Height); err != nil {
			return fmt.Errorf("--size WxH: %w", err)
		}
	}
	if o.image != "" {
		p, err := filepath.Abs(o.image)
		if err != nil {
			return err
		}
		r.Images = []string{p}
	}
	switch md.Kind {
	case engine.Chat:
		r.Messages = []llm.Message{{Role: "user", Content: text}}
		r.Temperature, r.MaxTokens, r.Think = float32(o.temp), o.maxTokens, o.think
	case engine.Embed:
		r.Texts = strings.Split(text, "|")
	case engine.Classify:
		return errors.New("the classifier: use the TUI or the web app")
	case engine.Sound:
		r.Prompt, r.Duration = text, o.duration
	case engine.Speech:
		r.Text, r.Voice, r.Language, r.Instruct, r.Speed = text, o.voice, o.language, o.instruct, o.speed
	case engine.Music:
		r.Prompt, r.Duration, r.Instrumental, r.BPM, r.Planner = text, o.duration, o.instrumental, o.bpm, o.planner
		if o.lyrics != "" {
			b, err := os.ReadFile(o.lyrics)
			if err != nil {
				return err
			}
			r.Lyrics = string(b)
		}
	default:
		r.Prompt = text
	}

	ch, stop := mgr.Subscribe()
	defer stop()
	j, err := mgr.Submit(r)
	if err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		_ = mgr.Cancel(j.ID)
	}()
	lastStep := -1
	for u := range ch {
		if u.JobID != j.ID {
			continue
		}
		if ev := u.Event; ev != nil {
			switch ev.Type {
			case "text":
				fmt.Print(ev.Text)
			case "status", "log":
				if !o.quiet {
					fmt.Fprintln(os.Stderr, "·", ev.Text)
				}
			case "progress":
				if !o.quiet && ev.Step != lastStep {
					lastStep = ev.Step
					fmt.Fprintf(os.Stderr, "· step %d/%d\n", ev.Step, ev.Total)
				}
			case "file":
				fmt.Println(ev.File)
			case "result":
				for k, v := range ev.Data {
					fmt.Printf("%s: %v\n", k, v)
				}
			}
		}
		if u.Job != nil && !u.Job.Active() {
			if md.Kind == engine.Chat {
				fmt.Println()
			}
			switch u.Job.State {
			case jobs.Failed:
				return errors.New(u.Job.Error)
			case jobs.Canceled:
				return errors.New("canceled")
			}
			if !o.quiet {
				fmt.Fprintln(os.Stderr, "·", u.Job.Status)
			}
			return nil
		}
	}
	return nil
}
