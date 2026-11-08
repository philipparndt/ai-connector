// mlxlab tries local AI models on a Mac with Apple silicon: the chat models, the embedder and the classifier of
// vboard (run in Go, on MLX and Accelerate/Metal), FLUX.2 klein (images, through mflux) and LTX-2.5 (video with
// audio, through ltx-2-mlx). The Mac is the worker: it dials the hub (on the Raspberry Pi), which signs people in
// and serves the web UI; the TUI and the iOS app talk to the hub (docs/architecture.md).
//
//	mlxlab worker [--hub URL]   serve this Mac's models to the hub
//	mlxlab serve                standalone on http://127.0.0.1:8484 (development, no hub)
//	mlxlab [tui]                the terminal UI, a client of the hub
//	mlxlab list                 the models
//	mlxlab run <model> <prompt> one job, output on the terminal
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/philipparndt/ai-connector/internal/approvals"
	"github.com/philipparndt/ai-connector/internal/engine"
	"github.com/philipparndt/ai-connector/internal/jobs"
	"github.com/philipparndt/ai-connector/internal/llm"
	"github.com/philipparndt/ai-connector/internal/models"
	"github.com/philipparndt/ai-connector/internal/prompts"
	"github.com/philipparndt/ai-connector/internal/tui"
	"github.com/philipparndt/ai-connector/internal/voices"
	"github.com/philipparndt/ai-connector/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mlxlab:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mlxlab — local AI models on MLX

usage:
  mlxlab worker [--hub URL] [--token T] [--config FILE] [--state DIR] [--install | --uninstall]
                                              serve this Mac's models to the hub (see docs/architecture.md)
  mlxlab serve [--addr 127.0.0.1:8484] [--state DIR]
                                              standalone on this Mac only: the web app and the API, no hub
  mlxlab [tui] [--server URL]                 the terminal UI, a client of the hub (or of mlxlab serve)
  mlxlab list                                 the models and where they are
  mlxlab run [options] <model> <prompt…>      one job; text goes to stdout, files are listed
  mlxlab mcp list | call <server__tool> [json] | serve transit the MCP servers of mcp.json and their tools (docs/chat-tools.md)
  mlxlab voices list | import <voices.yaml>   the speech models' voice library; import: a set of reference clips with their transcripts
  mlxlab agents login | status [--state DIR]  Claude Code's login as this worker's agent sessions use it (docs/api.md)
  ai-connector vboard <vboard address> --token T [--agents LIST]
                                              vboard's local AI on this computer, no server (ai-connector vboard --help)
  ai-connector local [--addr HOST:PORT] [--lan] [--name NAME] [--print-token] [--install | --uninstall]
                                              this computer's AI for its apps (OpenCAD, scripts), no server, no vboard;
                                              --lan: other computers too, Bonjour and PIN linking (ai-connector local
                                              --help). ai-connector serve --standalone is the same.

worker: the hub URL and the token come from the flags, $MLXLAB_HUB and $MLXLAB_WORKER_TOKEN, or worker.json
        (written when --hub and a token (flag or MLXLAB_WORKER_TOKEN) are given). The token is the hub's MLXLAB_WORKER_TOKEN.
        --config FILE ($MLXLAB_WORKER_CONFIG): another file than <files>/worker.json (a second worker on this Mac).
        --install: start it at login (a launchd agent de.rnd7.ai-connector.worker, a systemd user unit
        ai-connector-worker.service, a scheduled task "ai-connector worker"); hub and token are kept in the file,
        the service runs with --config/--state only (another --config: names of its own, e.g. worker-local.json →
        de.rnd7.ai-connector.worker.local). A copy that runs with the same file is stopped: the service takes over.
        --uninstall stops and removes it (with the same --config). One runs at a time per file (worker.lock beside it).
state:  --state DIR ($MLXLAB_STATE): this process's jobs (history.json, outputs/), prompts, approvals.json and
        Claude Code sessions (agents/, agents.json) go to DIR; the models, envs/, lib/, tools/, mcp.json, voices/
        and styles/ stay shared in <files>. Default: <files> itself.

run options: --size WxH --steps N --seed N --frames N --fps N --q 4|8|-1 --image FILE --temp T --think
             music: <model> "<style>" --lyrics FILE --duration SECONDS --instrumental --bpm N --planner 4b|off
             speech: <model> "<text>" --voice NAME --language de --instruct "calm" --speed 1

files: ~/.config/mlxlab (models/, envs/, outputs/, history.json, worker.json; $MLXLAB_HOME)
`)
}

func run() error {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch {
	case cmd == "tui":
		return tui.Main(args)
	case cmd == "mcp":
		return mcpCommand(args)
	case cmd == "voices":
		return voicesCommand(args)
	case cmd == "guard":
		return guardMain(args)
	case cmd == "agents":
		return agentsCommand(args)
	case cmd == "vboard":
		return vboardCommand(args)
	case cmd == "local":
		return localCommand(args)
	case cmd == "serve":
		if rest, ok := standaloneArgs(args); ok { // serve --standalone: the hub with auth and /v1 (local.go)
			return localCommand(rest)
		}
	case cmd == "" && !helpFlag(args):
		// plain mlxlab (and its flags, like --server) is the TUI
		return tui.Main(args)
	}
	fs := flag.NewFlagSet("mlxlab "+cmd, flag.ContinueOnError)
	fs.Usage = usage
	var addr, hub, token, cfgFile *string
	state := new(string)
	install, uninstall := new(bool), new(bool)
	var env envFlag
	switch cmd {
	case "serve":
		addr = fs.String("addr", "127.0.0.1:8484", "where the app listens (this Mac only)")
		state = fs.String("state", "", "this process's jobs, prompts and sessions (default: the config dir; $MLXLAB_STATE)")
	case "worker":
		hub = fs.String("hub", "", "the hub, e.g. https://mlxlab.example.org")
		token = fs.String("token", "", "the worker token (the hub's MLXLAB_WORKER_TOKEN)")
		cfgFile = fs.String("config", "", "the file with hub and token (default <config dir>/worker.json; $MLXLAB_WORKER_CONFIG)")
		state = fs.String("state", "", "this worker's jobs, prompts and sessions (default: the config dir; $MLXLAB_STATE)")
		install = fs.Bool("install", false, "start at login")
		uninstall = fs.Bool("uninstall", false, "do not start at login any more")
		// not in the usage: the Windows task's command carries the environment of --install with it (as vboard's)
		fs.Var(&env, "env", "KEY=VALUE, set before anything else (only the keys --install keeps)")
	}
	ro := runOptions{}
	if cmd == "run" {
		ro.flags(fs)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := applyEnv(env, os.Setenv); err != nil { // before anything reads the environment
		return err
	}

	root := engine.ConfigDir()
	own := stateDir(*state, os.Getenv, root)
	switch cmd {
	case "worker":
		file := workerFile(*cfgFile, os.Getenv, root)
		var svc service
		if *uninstall || *install { // install.go
			var err error
			if svc, err = newService(os.Getenv, workerKind(file, filepath.Join(root, "worker.json"), *state), workerLock(file)); err != nil {
				return err
			}
			if *uninstall {
				if err := svc.uninstall(); err != nil {
					return err
				}
				fmt.Println(svc.k().what, "does not start at login any more")
				return nil
			}
		}
		cfg, save, err := resolveWorker(*hub, *token, os.Getenv, file)
		if err != nil {
			if *install {
				return fmt.Errorf("--install needs a hub and a token (--hub and --token, $MLXLAB_HUB and $MLXLAB_WORKER_TOKEN, or %s): %w", file, err)
			}
			return err
		}
		allowLocalRuntimes, idleUnload, pinned = cfg.AllowLocalRuntimes, cfg.IdleUnload, cfg.Pin
		if save || *install { // the service has neither flags nor this environment: hub and token only from the file
			if err := saveWorker(file, cfg); err != nil {
				return err
			}
			fmt.Println("mlxlab worker: hub and token kept in", file)
		}
		if *install {
			if err := svc.install(); err != nil {
				return err
			}
			fmt.Println(svc.describe())
			return nil
		}
		// one at a time per config file (instance.go): another one is waited for, not given up on
		lock, err := takeWorkerLock(file)
		if err != nil || lock == nil {
			return err
		}
		defer lock.Close()
		_, mgr := setup(root, own)
		return worker(mgr, cfg, web.HostName(), (&web.Server{Jobs: mgr, Prompts: prompts.Open(mgr.Dir), Home: root, HomeTools: homeTools, Version: version, Agents: claudeAgents,
			Downloads: downloads(root, mgr.Catalog)}).Handler()) // a server's vboard connector downloads on request (vboard_setup.go)
	case "serve":
		_, mgr := setup(root, own)
		url, err := serve(mgr, *addr)
		if err != nil {
			return err
		}
		fmt.Println("mlxlab:", url)
		ch, stop := mgr.Subscribe()
		defer stop()
		go logUpdates(ch)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		mgr.Unload()
		claudeAgents.Close()
		closeSandboxes()
		return nil
	case "list":
		cat, _ := setup(root, own)
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tKIND\tBACKEND\tSIZE\tWHERE")
		for _, m := range cat.List() {
			where := "↓ on first use"
			if m.Here {
				where = m.Where
			}
			if m.Gated && !m.Here {
				where += " (gated: HF_TOKEN)"
			}
			size := "–"
			if m.Size > 0 {
				size = models.Human(m.Size)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", m.ID, m.Kind, m.Backend, size, where)
		}
		return w.Flush()
	case "run":
		_, mgr := setup(root, own)
		return ro.run(mgr, fs.Args())
	case "", "help":
		usage()
		return nil
	}
	usage()
	return fmt.Errorf("unknown command %q", cmd)
}

func helpFlag(args []string) bool {
	for _, a := range args {
		switch a {
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}

// appleSilicon: the MLX chat models and the Python runtimes (mflux, ltx-2-mlx, mlx-audio, …) run here.
const appleSilicon = runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"

// setup builds the catalogue, the backends and the job manager: the models, envs and libraries from root (the
// config dir, shared), the jobs, prompts, approvals and Claude Code sessions in own (stateDir; root by default).
func setup(root, own string) (*engine.Catalog, *jobs.Manager) {
	engine.EndLeftovers(root) // servers of a worker that died, with their models in memory
	srcs, _ := models.Sources(os.Getenv("MLXLAB_MODELS_FROM"), nil)
	store := &models.Store{Dir: filepath.Join(root, "models"), Sources: srcs}
	lms, hf := llm.LocalRoots()
	finder := &llm.Finder{Store: store, LMStudio: lms, HFCache: hf, Mem: llm.MemBytes(),
		Verifier: &llm.Verifier{Path: filepath.Join(store.Dir, "verified.json")}}
	// this machine's own Python runtimes (<config dir>/runtimes.d), when allowed: they run code nobody reviewed
	if allowLocalRuntimes || os.Getenv("MLXLAB_ALLOW_LOCAL_RUNTIMES") == "1" {
		for _, err := range engine.AddLocalRuntimes(filepath.Join(root, "runtimes.d")) {
			fmt.Fprintln(os.Stderr, "mlxlab: runtimes.d:", err)
		}
	}
	cat := engine.NewCatalog(store, finder)
	// this machine's own models: manifests in <config dir>/models.d (docs/models.md)
	for _, err := range cat.AddLocal(filepath.Join(root, "models.d"), store) {
		fmt.Fprintln(os.Stderr, "mlxlab: models.d:", err)
	}
	text := &engine.TextBackend{Store: store}
	// the tools allowed always (approvals.json): the chats ask it, the web API changes it
	always := approvals.Open(own)
	// speech is also the avatar backend's Speaker (it says the avatar's text in its own job)
	speech := &engine.SpeechBackend{Root: root, Store: store, Catalog: cat, Voices: voices.Open(filepath.Join(root, "voices"))}
	// the embedder and the classifier run everywhere (pure Go), the chat models on Apple silicon (MLX); the Python
	// backends where their runtime has an environment for this machine (runtimes/*.json: BiRefNet on PyTorch with
	// CUDA, ROCm, MPS or the CPU; the MLX ones on Apple silicon), each model where its own runtime has one
	backends := []engine.Backend{text}
	if appleSilicon {
		backends = append(backends, &engine.ChatBackend{Finder: finder, Agent: agentConfig(own, text, always)})
	}
	for _, b := range []struct {
		runs    bool
		backend engine.Backend
	}{
		{engine.BackendRuns("cutout"), &engine.CutoutBackend{Root: root, Store: store}},
		{engine.BackendRuns("mflux"), &engine.FluxBackend{Root: root, Store: store}},
		{engine.BackendRuns("ltx"), &engine.LTXBackend{Root: root, Store: store}},
		{engine.BackendRuns("music"), &engine.MusicBackend{Root: root, Store: store, Runtimes: engine.MusicRuntimes()}},
		{engine.BackendRuns("speech"), speech},
		{engine.BackendRuns("sound"), &engine.SoundBackend{Root: root, Store: store}},
		// the avatar says its text with the speech backend
		{engine.BackendRuns("avatar"), &engine.AvatarBackend{Root: root, Store: store, Speaker: speech}},
	} {
		if b.runs {
			backends = append(backends, b.backend)
		}
	}
	for _, n := range engine.GenericRuntimes() {
		backends = append(backends, &engine.RuntimeBackend{Root: root, Store: store, Runtime: n})
	}
	// servers the worker starts with their model (mlx-lm, llama.cpp: runtimes/*.json with server)
	for _, n := range engine.ServerRuntimes("openai") {
		backends = append(backends, &engine.ServerBackend{Root: root, Store: store, Runtime: n})
	}
	// ComfyUI: workflows as models (runtimes/comfyui.json, Protocol comfyui)
	for _, n := range engine.ServerRuntimes("comfyui") {
		backends = append(backends, engine.NewComfyBackend(root, store, n))
	}
	// one backend a name: the first stays (a runtime manifest never takes a built-in backend's models over)
	runs := map[string]bool{}
	kept := backends[:0]
	for _, b := range backends {
		if runs[b.Name()] {
			fmt.Fprintf(os.Stderr, "backend %s is there twice: the second is left out\n", b.Name())
			continue
		}
		runs[b.Name()] = true
		kept = append(kept, b)
	}
	backends = kept
	cat.Keep(func(m *engine.Model) bool { return runs[m.Backend] && engine.ModelRuns(m) })
	// local OpenAI-compatible servers (Ollama, LM Studio, llama.cpp, vLLM): their models, found now and every 30 s
	rts, err := engine.ParseRuntimes(os.Getenv("MLXLAB_RUNTIMES"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "mlxlab:", err)
	}
	if len(rts) > 0 {
		oa := &engine.OpenAIBackend{Catalog: cat, Runtimes: rts}
		oa.Discover(context.Background())
		go oa.Watch(context.Background(), 30*time.Second)
		backends = append(backends, oa)
	}
	// Claude Code, when it is installed here (internal/agents): its jobs run beside the queue
	backends = append(backends, claudeBackend(root, own, cat))
	mgr := jobs.New(cat, backends, own)
	// when the worker says so: a model with no job for a while leaves memory (the machine is someone's too), but the
	// pinned ones
	idle, pins := idleSettings(os.Getenv)
	go mgr.WatchIdle(idle, pins, nil)
	mgr.Approvals = always
	return cat, mgr
}

// serve runs the app standalone on this Mac. It has no sign-in, so it never listens beyond localhost: other devices
// go through the hub.
func serve(mgr *jobs.Manager, addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("cannot listen on %s: %w (--addr)", addr, err)
	}
	if !loopback(ln.Addr()) {
		ln.Close()
		return "", fmt.Errorf("%s is reachable from other devices, and mlxlab serve has no sign-in: use 127.0.0.1, "+
			"and mlxlab worker with the hub for other devices (docs/architecture.md)", addr)
	}
	h := web.SameOrigin((&web.Server{Jobs: mgr, Prompts: prompts.Open(mgr.Dir), Home: engine.ConfigDir(), HomeTools: homeTools, Version: version, Agents: claudeAgents}).Handler())
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), nil
}

func loopback(a net.Addr) bool {
	t, ok := a.(*net.TCPAddr)
	return ok && t.IP.IsLoopback()
}

func logUpdates(ch <-chan jobs.Update) {
	for u := range ch {
		switch {
		case u.Job != nil:
			j := u.Job
			line := fmt.Sprintf("%s %s %s: %s", time.Now().Format("15:04:05"), j.ID, j.Model, j.Status)
			if j.Error != "" {
				line += " — " + j.Error
			}
			fmt.Println(line)
		case u.Event != nil && (u.Event.Type == "log" || u.Event.Type == "status"):
			fmt.Printf("%s %s   %s\n", time.Now().Format("15:04:05"), u.JobID, u.Event.Text)
		}
	}
}
