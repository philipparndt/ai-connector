package engine

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/philipparndt/ai-connector/internal/pyenv"
)

// Python runtimes as data (docs/models.md, "Runtimes"): a runtime is a JSON manifest — its worker script, an optional
// build step, and one environment per platform (the packages differ: PyTorch's CUDA or ROCm wheels, MLX only on Apple
// silicon). A worker offers a runtime's models only where the runtime has an environment for its platform.

//go:embed runtimes/*.json
var runtimeFiles embed.FS

// RuntimeManifest is one Python runtime.
type RuntimeManifest struct {
	Name   string                `json:"name"`
	Notes  string                `json:"notes,omitempty"`
	Script string                `json:"script,omitempty"` // in workers/ (a local runtime: beside its manifest)
	Build  string                `json:"build,omitempty"`  // a build step of its own (levo2, sa3)
	Envs   map[string]RuntimeEnv `json:"envs"`             // by platform (Platform)
	// Generic: run by the generic backend (RuntimeBackend) — the job protocol with the request as it comes, no Go of
	// its own; Kinds are what its models may be, Output the extension of what it makes
	Generic bool   `json:"generic,omitempty"`
	Kinds   []Kind `json:"kinds,omitempty"`
	Output  string `json:"output,omitempty"`
	// Heavy: its model takes so much memory that the queue unloads the other heavy ones first (default: yes; false for
	// a small one that may stay beside them)
	Heavy *bool `json:"heavy,omitempty"`
	// Server: a program that serves an OpenAI-compatible API, started by the worker with the model (ServerBackend);
	// its program comes from Envs (a Python package's script) or Tools (archives of a release, by platform)
	Server *ServerSpec              `json:"server,omitempty"`
	Tools  map[string][]ToolArchive `json:"tools,omitempty"`

	dir string // a local runtime's directory (its script is there)
}

// ServerSpec is how a server runtime starts: Start is the program and its arguments, with {model} (the model's
// file or folder), {dir} (its folder) and {port}; KeyEnv the environment variable the program takes an API key from
// (a random one of this start, which the worker sends; never an argument: those every user of the machine can read);
// Health a path that answers 200 once the model is loaded; Model what a request names as the model ("" the {model}
// path).
type ServerSpec struct {
	Start  []string `json:"start"`
	Health string   `json:"health"`
	Model  string   `json:"model,omitempty"`
	KeyEnv string   `json:"keyEnv,omitempty"`
	// Protocol: how the worker asks it — "" or "openai" (ServerBackend), "comfyui" (workflows, ComfyBackend)
	Protocol string `json:"protocol,omitempty"`
	// App: a file of the runtime's tools the program runs ({app}, its folder the working directory): ComfyUI's
	// main.py, run by the environment's python
	App string `json:"app,omitempty"`
}

// ToolArchive is an archive of a program's release (.tar.gz or .zip), pinned by its SHA-256. Arch is the processor
// its programs are built for (GOARCH); "" a source archive that runs on any.
type ToolArchive struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Arch   string `json:"arch,omitempty"`
}

// toolsFor are the runtime's archives for a platform and processor (nil: a platform says the system and its GPU, not
// the processor, and one built for another could not start here).
func (rt *RuntimeManifest) toolsFor(platform, goarch string) []ToolArchive {
	as := rt.Tools[platform]
	for _, a := range as {
		if a.Arch != "" && a.Arch != goarch {
			return nil
		}
	}
	return as
}

// tools are the runtime's archives for this machine.
func (rt *RuntimeManifest) tools() []ToolArchive { return rt.toolsFor(Platform(), runtime.GOARCH) }

// RuntimeEnv is a runtime's environment on one platform.
type RuntimeEnv struct {
	Python   string   `json:"python"`
	Packages []string `json:"packages,omitempty"`
	// Index: a package index for the packages IndexPackages names only (installed from it alone; the rest from PyPI)
	Index         string   `json:"index,omitempty"`
	IndexPackages []string `json:"indexPackages,omitempty"`
}

// Platforms a runtime can have an environment for.
var Platforms = []string{"darwin-arm64", "linux-cuda", "linux-rocm", "linux-cpu", "windows-cuda", "windows-rocm", "windows-cpu"}

var runtimeBuilds = map[string]bool{"levo2": true, "sa3": true}

// ParseRuntime reads one runtime manifest and checks it.
func ParseRuntime(data []byte) (*RuntimeManifest, error) {
	var rt RuntimeManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rt); err != nil {
		return nil, err
	}
	switch {
	case !manifestID.MatchString(rt.Name):
		return nil, fmt.Errorf("name %q: lower case letters, digits, '.', '_' and '-'", rt.Name)
	case len(rt.Envs) == 0 && len(rt.Tools) == 0:
		return nil, fmt.Errorf("%s: envs or tools (one per platform) are needed", rt.Name)
	case rt.Server != nil && (len(rt.Server.Start) == 0 || rt.Server.Health == "" || len(rt.Kinds) == 0):
		return nil, fmt.Errorf("%s: a server needs start, health and kinds", rt.Name)
	case rt.Server != nil && strings.Contains(strings.Join(rt.Server.Start, " "), "{key}"):
		return nil, fmt.Errorf("%s: no key in the arguments (every user of the machine can read them): keyEnv", rt.Name)
	case rt.Server != nil && rt.Server.Protocol != "" && rt.Server.Protocol != "openai" && rt.Server.Protocol != "comfyui":
		return nil, fmt.Errorf("%s: protocol %q is none of openai, comfyui", rt.Name, rt.Server.Protocol)
	case rt.Server != nil && rt.Generic:
		return nil, fmt.Errorf("%s: a runtime is a server or generic, not both", rt.Name)
	case rt.Build != "" && !runtimeBuilds[rt.Build]:
		return nil, fmt.Errorf("%s: no build step %q", rt.Name, rt.Build)
	case rt.Generic && (rt.Script == "" || len(rt.Kinds) == 0 || rt.Output == ""):
		return nil, fmt.Errorf("%s: a generic runtime needs script, kinds and output", rt.Name)
	case rt.Generic && (rt.Envs["windows-cuda"].Python != "" || rt.Envs["windows-rocm"].Python != "" || rt.Envs["windows-cpu"].Python != ""):
		return nil, fmt.Errorf("%s: a generic runtime cannot run on Windows yet (the job protocol's events come on file descriptor 3)", rt.Name)
	case rt.Generic && !manifestID.MatchString(strings.TrimSuffix(rt.Script, ".py")):
		return nil, fmt.Errorf("%s: script %q: a file name beside the manifest", rt.Name, rt.Script)
	}
	for p, as := range rt.Tools {
		if !contains(Platforms, p) {
			return nil, fmt.Errorf("%s: platform %q is none of %s", rt.Name, p, strings.Join(Platforms, ", "))
		}
		for _, a := range as {
			if !strings.HasPrefix(a.URL, "https://") || len(a.SHA256) != 64 {
				return nil, fmt.Errorf("%s: %s: each tool archive needs an https url and its sha256", rt.Name, p)
			}
		}
	}
	for p, e := range rt.Envs {
		if !contains(Platforms, p) {
			return nil, fmt.Errorf("%s: platform %q is none of %s", rt.Name, p, strings.Join(Platforms, ", "))
		}
		if e.Python == "" {
			return nil, fmt.Errorf("%s: %s: python (the version) is needed", rt.Name, p)
		}
		if (e.Index == "") != (len(e.IndexPackages) == 0) {
			return nil, fmt.Errorf("%s: %s: index and indexPackages go together (the packages taken from the index)", rt.Name, p)
		}
	}
	return &rt, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

var (
	runtimesMu sync.RWMutex
	runtimes   = sync.OnceValue(func() map[string]*RuntimeManifest {
		out := map[string]*RuntimeManifest{}
		names, _ := fs.Glob(runtimeFiles, "runtimes/*.json")
		for _, n := range names {
			b, _ := runtimeFiles.ReadFile(n)
			rt, err := ParseRuntime(b)
			if err != nil {
				panic(fmt.Sprintf("the embedded runtimes: %s: %v", n, err))
			}
			out[rt.Name] = rt
		}
		return out
	})
)

// runtimeManifest is the runtime name (nil: none).
func runtimeManifest(name string) *RuntimeManifest {
	runtimesMu.RLock()
	defer runtimesMu.RUnlock()
	return runtimes()[name]
}

// runtimeNames are the runtimes there are, sorted.
func runtimeNames() []string {
	runtimesMu.RLock()
	defer runtimesMu.RUnlock()
	var out []string
	for n := range runtimes() {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// envFor is the runtime's environment on a platform (ok false: it has none there).
func envFor(name, platform string) (pyenv.Env, bool) {
	rt := runtimeManifest(name)
	if rt == nil {
		return pyenv.Env{}, false
	}
	e, ok := rt.Envs[platform]
	if !ok {
		return pyenv.Env{}, false
	}
	return pyenv.Env{Name: name, Python: e.Python, Packages: e.Packages, Index: e.Index, IndexPackages: e.IndexPackages}, true
}

// runtimeEnv is the runtime's environment on this machine (the zero Env where it has none: RuntimeRuns says).
func runtimeEnv(name string) pyenv.Env {
	e, _ := envFor(name, Platform())
	return e
}

// RuntimeRuns: the runtime has what it needs on this machine: its environment, its tools, or both where it names both.
func RuntimeRuns(name string) bool {
	rt := runtimeManifest(name)
	if rt == nil {
		return false
	}
	_, env := envFor(name, Platform())
	tools := len(rt.tools()) > 0
	switch {
	case len(rt.Envs) > 0 && len(rt.Tools) > 0:
		return env && tools
	}
	return env || tools
}

// Platform is this machine as the runtimes name it: darwin-arm64 (Apple silicon), else the system and its GPU —
// linux-cuda, linux-rocm, linux-cpu, windows-cuda, windows-rocm, windows-cpu. MLXLAB_GPU (cuda, rocm, cpu) overrides
// what is found: an NVIDIA driver (nvidia-smi), AMD's ROCm (rocminfo, /opt/rocm; Windows: the HIP SDK's HIP_PATH).
var Platform = sync.OnceValue(func() string { return platformOf(runtime.GOOS, runtime.GOARCH, os.Getenv("MLXLAB_GPU"), haveGPU) })

func platformOf(goos, goarch, gpu string, have func(string) bool) string {
	if goos == "darwin" && goarch == "arm64" {
		return "darwin-arm64"
	}
	if gpu == "" {
		switch {
		case have("cuda"):
			gpu = "cuda"
		case have("rocm"):
			gpu = "rocm"
		default:
			gpu = "cpu"
		}
	}
	return goos + "-" + gpu
}

func haveGPU(kind string) bool {
	switch kind {
	case "cuda":
		_, err := exec.LookPath("nvidia-smi")
		return err == nil
	case "rocm":
		if runtime.GOOS == "windows" {
			return os.Getenv("HIP_PATH") != ""
		}
		if _, err := exec.LookPath("rocminfo"); err == nil {
			return true
		}
		_, err := os.Stat("/opt/rocm")
		return err == nil
	}
	return false
}

// BuiltinBackends are the names of the backends in Go (the backend a model names, Backend.Name()): a local runtime
// with one of them would take its models over, so none may have one.
var BuiltinBackends = map[string]bool{"go": true, "mlx-go": true, "cutout": true, "mflux": true, "ltx": true, "music": true,
	"speech": true, "sound": true, "avatar": true, BackendClaudeCode: true, BackendOpenAI: true}

// AddLocalRuntimes adds the runtimes of dir (<config dir>/runtimes.d/<name>/runtime.json with its script beside it):
// a machine's own Python runtimes, run by the generic backend. Only when the worker allows them
// (allowLocalRuntimes: they run code nobody reviewed); a runtime the catalogue has already is left out.
func AddLocalRuntimes(dir string) []error {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return []error{err}
	}
	var errs []error
	runtimesMu.Lock()
	defer runtimesMu.Unlock()
	all := runtimes()
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		d := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(filepath.Join(d, "runtime.json"))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		rt, err := ParseRuntime(b)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
		case !rt.Generic:
			errs = append(errs, fmt.Errorf("%s: a local runtime is a generic one (generic: true)", e.Name()))
		case all[rt.Name] != nil:
			errs = append(errs, fmt.Errorf("%s: the catalogue has a runtime %s already; left out", e.Name(), rt.Name))
		case BuiltinBackends[rt.Name]:
			errs = append(errs, fmt.Errorf("%s: %s is a built-in backend: a local runtime cannot take its name; left out", e.Name(), rt.Name))
		default:
			if _, err := os.Stat(filepath.Join(d, rt.Script)); err != nil {
				errs = append(errs, fmt.Errorf("%s: its script: %w", e.Name(), err))
				continue
			}
			rt.dir = d
			all[rt.Name] = rt
		}
	}
	return errs
}

// heavy says whether its models count as heavy (Heavy, default yes).
func (rt *RuntimeManifest) heavy() bool { return rt.Heavy == nil || *rt.Heavy }

// script is the runtime's worker script: its source.
func (rt *RuntimeManifest) source() ([]byte, error) {
	if rt.dir != "" {
		return os.ReadFile(filepath.Join(rt.dir, rt.Script))
	}
	return workerFiles.ReadFile("workers/" + rt.Script)
}

// runtimeName is the Python runtime a model runs in ("" none: Go, a local server, Claude Code).
func runtimeName(m *Model) string {
	switch m.Backend {
	case "mflux", "cutout", "sound":
		return m.Backend
	case "speech":
		return speechRuntime(m.workerModel) // a family may have a runtime of its own (Fish S2 Pro: fish-speech)
	case "ltx":
		return "ltx-2-mlx"
	case "music":
		return m.runtime
	case "avatar":
		if m.runtime == "longcat" {
			return "longcat-avatar"
		}
		return "ltx-2-mlx"
	}
	if rt := runtimeManifest(m.Backend); rt != nil && (rt.Generic || rt.Server != nil) {
		return m.Backend
	}
	return ""
}

// ModelRuns: the model's Python runtime (if it has one) has an environment for this machine.
func ModelRuns(m *Model) bool {
	n := runtimeName(m)
	return n == "" || RuntimeRuns(n)
}

// packageCommit is the commit a runtime's package is pinned to ("name @ git+https://…@<commit>…") on Apple silicon.
func packageCommit(runtime, pkg string) string {
	e, _ := envFor(runtime, "darwin-arm64")
	for _, p := range e.Packages {
		if !strings.HasPrefix(p, pkg+" @ ") {
			continue
		}
		at := strings.LastIndex(p, "@")
		c := p[at+1:]
		if i := strings.IndexAny(c, "#&"); i >= 0 {
			c = c[:i]
		}
		return c
	}
	panic("runtime " + runtime + " pins no package " + pkg)
}

// GenericRuntimes are the generic runtimes with an environment for this machine (each gets a RuntimeBackend).
func GenericRuntimes() []string {
	var out []string
	for _, n := range runtimeNames() {
		if rt := runtimeManifest(n); rt.Generic && RuntimeRuns(n) {
			out = append(out, n)
		}
	}
	return out
}

// ServerRuntimes are the server runtimes this machine can start that speak protocol ("openai": a ServerBackend each,
// "comfyui": a ComfyBackend each).
func ServerRuntimes(protocol string) []string {
	var out []string
	for _, n := range runtimeNames() {
		rt := runtimeManifest(n)
		if rt.Server == nil || !RuntimeRuns(n) {
			continue
		}
		if p := rt.Server.Protocol; p == protocol || (p == "" && protocol == "openai") {
			out = append(out, n)
		}
	}
	return out
}

// BackendRuns: some model of the catalogue's of a backend can run here (its runtime has an environment or its tools for
// this machine) — so the worker offers the backend (music: if any song runtime runs).
func BackendRuns(backend string) bool {
	for _, mf := range builtinManifests() {
		if mf.Backend == backend && ModelRuns(mf.model(nil, "")) {
			return true
		}
	}
	return false
}
