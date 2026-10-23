package agents

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Drivers.
const (
	DriverJSON = "json"
	DriverTTY  = "tty"
)

// Session states.
const (
	StateStarting = "starting"
	StateIdle     = "idle"
	StateBusy     = "busy"
	StateClosed   = "closed"
)

// MCPServer is an MCP server a session may use (its tools are allowed as mcp__<name>__*).
type MCPServer struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Type    string            `json:"type,omitempty"` // "http" (streamable HTTP, the default) or "sse"
	Headers map[string]string `json:"headers,omitempty"`
}

// Options say which session a turn belongs to and how it is started.
type Options struct {
	// Key is the conversation: turns with the same owner and key share one session. "" is a one-off session,
	// closed (and its folder removed) after its turn.
	Key    string
	Owner  string
	Model  string // a Claude Code model alias or name ("" the Config's)
	Driver string // DriverJSON ("") or DriverTTY
	MCP    []MCPServer
	// System is appended to Claude Code's system prompt when the session starts.
	System string
	// Cwd: a person's tty session may work in a folder of theirs ("" the session's own folder).
	Cwd string
	// Fresh starts the conversation anew: a running session of the key is closed and nothing is resumed (for a
	// client whose conversation key can repeat, e.g. a new chat that opens with the same question as an old one).
	Fresh bool
	// Opening is sent instead of the turn's text while the conversation is new on this worker (no session of the
	// key ran a turn, nothing to resume): the earlier messages as a transcript in front of the question, so that a
	// conversation this worker never saw (or lost) still has its history.
	Opening string
	// Images go with the turn's text, OpeningImages with the Opening (images.go): each where the text names it
	// ("[image-1]").
	Images, OpeningImages []Image
	// Effort is Claude Code's effort level (--effort: one of Efforts; "" the Config's, else Claude Code's default).
	Effort string
}

// Event is what happens during a turn.
type Event struct {
	Type string    // "text" (a piece of the answer), "tool" (a tool call started or ended), "status"
	Text string    // text, status
	Tool *ToolCall // tool
}

// ToolCall is one of Claude Code's tool calls.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	State     string          `json:"state"` // running | done | error
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Summary   string          `json:"summary,omitempty"` // the input in a few words (a path, a command, a URL)
	Result    string          `json:"result,omitempty"`  // shortened
}

// Result is how a turn ended.
type Result struct {
	Text             string  // the final answer
	CostUSD          float64 // this turn's (Claude Code reports the session's total; the difference)
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	Duration         time.Duration
	FirstText        time.Duration // from the turn's start to its first text (json driver)
	Turns            int           // model requests in the turn (tool rounds)
	ClaudeSession    string        // Claude Code's session id (resumable)
	Session          string        // our session id
	Driver           string
}

// Info is a session as GET /api/agents/sessions lists it.
type Info struct {
	ID            string    `json:"id"`
	Conversation  string    `json:"conversation,omitempty"`
	Model         string    `json:"model"`
	Driver        string    `json:"driver"`
	State         string    `json:"state"`
	Created       time.Time `json:"created"`
	LastUsed      time.Time `json:"lastUsed"`
	Cwd           string    `json:"cwd"`
	Owner         string    `json:"owner,omitempty"`
	Viewers       int       `json:"viewers"`
	Turns         int       `json:"turns"`
	ClaudeSession string    `json:"claudeSession,omitempty"`
}

// Errors.
var (
	ErrUnavailable = errors.New("Claude Code is not installed on this worker (claude is not on the PATH)")
	ErrTooMany     = errors.New("too many Claude Code sessions are busy on this worker")
	ErrNoSession   = errors.New("no such session")
	ErrNotTTY      = errors.New("not a tty session")
	ErrTTYWindows  = errors.New("tty sessions are not supported on Windows yet (use the json driver)")
)

// driver runs one Claude Code process.
type driver interface {
	turn(ctx context.Context, text string, emit func(Event)) (Result, error)
	close()
	exited() <-chan struct{}
	claudeSession() string
}

// Manager keeps the sessions of this worker.
type Manager struct {
	Root string // the sessions' folders: <config>/agents
	// Home is the config folder around Root (~/.config/mlxlab): sessions may read nothing of it but their own
	// folder (guard.go). "" in tests.
	Home string
	// Shared are more config folders a session may read nothing of (the config dir when this worker keeps its own
	// state elsewhere: mlxlab worker --state); guarded like Home.
	Shared  []string
	Config  Config
	Command string   // "claude"
	Env     []string // added to the environment of claude
	// Version, when set, is taken as `claude --version` (tests, and a known install); else it is asked.
	Version string

	mu       sync.Mutex
	sessions map[string]*Session
	resume   map[string]string // session id → Claude Code's session id: a closed conversation continues

	probeMu   sync.Mutex
	probed    time.Time
	version   string
	flags     string // claude --help, to see which options this version has
	probeErr  error
	logins    map[string]loginState // `claude auth status` per environment (home.go)
	reapEvery time.Duration
}

// New makes the manager of <configDir>/agents with <configDir>/agents.json.
func New(configDir string) (*Manager, error) {
	cfg, err := LoadConfig(configDir)
	m := &Manager{Root: filepath.Join(configDir, "agents"), Home: configDir, Config: cfg, Command: "claude"}
	return m, err
}

func (m *Manager) init() {
	if m.sessions == nil {
		m.sessions = map[string]*Session{}
		m.resume = map[string]string{}
		if b, err := os.ReadFile(filepath.Join(m.Root, "resume.json")); err == nil {
			json.Unmarshal(b, &m.resume)
		}
		if m.Command == "" {
			m.Command = "claude"
		}
		m.Config = m.Config.withDefaults()
		m.pruneProjects()
	}
}

// Available says whether Claude Code is installed here (`claude --version`), and its version. Asked once a minute
// at most (it may be installed while the worker runs).
func (m *Manager) Available() (string, bool) {
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	if m.Version != "" {
		return m.Version, true
	}
	if !m.probed.IsZero() && (m.probeErr == nil || time.Since(m.probed) < time.Minute) {
		return m.version, m.probeErr == nil
	}
	m.probed = time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := m.command()
	out, err := exec.CommandContext(ctx, cmd, "--version").Output()
	if err != nil {
		m.version, m.probeErr = "", ErrUnavailable
		return "", false
	}
	m.version, m.probeErr = strings.TrimSpace(string(out)), nil
	help, _ := exec.CommandContext(ctx, cmd, "--help").CombinedOutput()
	m.flags = string(help)
	return m.version, true
}

// has: this claude knows the option (from --help; unknown when the version was given: assumed yes).
func (m *Manager) has(flag string) bool {
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	return m.flags == "" || strings.Contains(m.flags, flag)
}

func (m *Manager) command() string {
	if m.Command == "" {
		return "claude"
	}
	return m.Command
}

// Session is one Claude Code process and the conversation it holds.
type Session struct {
	m       *Manager
	id      string
	opts    Options
	cwd     string   // where Claude Code works
	ctl     string   // the session's own settings, MCP config and hook log (not inside cwd)
	env     []string // added to claude's environment: the MCP secrets (guard.go)
	home    []string // the part of env for Claude Code's own files (home.go): its login is the same for each
	created time.Time
	resumed bool // started with --resume: it continues a conversation
	start   sync.Once
	ready   chan struct{} // closed when the process started (or failed to)
	err     error

	turnMu sync.Mutex // one turn at a time

	mu       sync.Mutex
	drv      driver
	lastUsed time.Time
	busy     bool
	closed   bool
	turns    int
}

// ID is the session's id (route-safe: lower-case letters and digits).
func (s *Session) ID() string { return s.id }

func (s *Session) info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := StateIdle
	switch {
	case s.closed:
		st = StateClosed
	case s.drv == nil:
		st = StateStarting
	case s.busy:
		st = StateBusy
	}
	in := Info{ID: s.id, Conversation: s.opts.Key, Model: s.opts.Model, Driver: s.opts.Driver, State: st, Created: s.created,
		LastUsed: s.lastUsed, Cwd: s.cwd, Owner: s.opts.Owner, Turns: s.turns}
	if s.drv != nil {
		in.ClaudeSession = s.drv.claudeSession()
		if t, ok := s.drv.(*ttyDriver); ok {
			in.Viewers = t.viewerCount()
		}
	}
	return in
}

// dead: closed, or its process ended.
func (s *Session) dead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return true
	}
	if s.drv == nil {
		return false
	}
	select {
	case <-s.drv.exited():
		return true
	default:
		return false
	}
}

// sessionID: the same owner and conversation give the same id (and so the same folder, which Claude Code's
// --resume needs); a one-off session gets a random one.
func sessionID(owner, key string) string {
	if key == "" {
		b := make([]byte, 6)
		rand.Read(b)
		return "o" + hex.EncodeToString(b)
	}
	h := sha256.Sum256([]byte(owner + "\x00" + key))
	return "s" + hex.EncodeToString(h[:6])
}

// ValidID: a session id as this package makes them.
func ValidID(id string) bool {
	if len(id) != 13 || (id[0] != 's' && id[0] != 'o' && id[0] != 'p') {
		return false
	}
	_, err := hex.DecodeString(id[1:])
	return err == nil
}

func (m *Manager) normalize(o Options) Options {
	m.mu.Lock()
	m.init()
	m.mu.Unlock()
	if o.Model == "" {
		o.Model = m.Config.Model
	}
	if o.Driver == "" {
		o.Driver = DriverJSON
	}
	if o.Effort == "" {
		o.Effort = m.Config.Effort
	}
	return o
}

// open finds the session of the options or starts one.
func (m *Manager) open(o Options, id string) (*Session, error) {
	if _, ok := m.Available(); !ok {
		return nil, ErrUnavailable
	}
	if o.Driver != DriverJSON && o.Driver != DriverTTY {
		return nil, fmt.Errorf("unknown driver %q (json or tty)", o.Driver)
	}
	if !ValidEffort(o.Effort) {
		return nil, fmt.Errorf("unknown effort %q (%s)", o.Effort, strings.Join(Efforts, ", "))
	}
	m.mu.Lock()
	m.init()
	if id == "" {
		id = sessionID(o.Owner, o.Key)
	}
	var stale *Session
	if s := m.sessions[id]; s != nil {
		if !o.Fresh && !s.dead() && s.opts.Model == o.Model && s.opts.Driver == o.Driver && s.opts.Effort == o.Effort &&
			sameMCP(s.opts.MCP, o.MCP) {
			s.mu.Lock()
			s.lastUsed = time.Now()
			s.mu.Unlock()
			m.mu.Unlock()
			<-s.ready
			return s, s.err
		}
		stale = s // another model, driver, effort or MCP servers: a new process continues the conversation (Fresh: not)
		delete(m.sessions, id)
	}
	if _, ok := m.resume[id]; ok && o.Fresh {
		delete(m.resume, id)
		_ = writeJSONFile(filepath.Join(m.Root, "resume.json"), m.resume)
	}
	if stale == nil {
		if victim, err := m.makeRoomLocked(); err != nil {
			m.mu.Unlock()
			return nil, err
		} else if victim != nil {
			defer victim.shut()
		}
	}
	now := time.Now()
	s := &Session{m: m, id: id, opts: o, created: now, lastUsed: now, ready: make(chan struct{}),
		cwd: filepath.Join(m.Root, id), ctl: filepath.Join(m.Root, ".ctl", id)}
	if o.Cwd != "" {
		s.cwd = o.Cwd
	}
	m.sessions[id] = s
	resume := m.resume[id]
	s.resumed = resume != ""
	m.mu.Unlock()
	if stale != nil {
		stale.turnMu.Lock() // its turn (if any) ends first
		stale.shut()
		stale.turnMu.Unlock()
	}
	s.start.Do(func() {
		defer close(s.ready)
		drv, err := m.startDriver(s, resume)
		s.mu.Lock()
		s.drv, s.err = drv, err
		s.mu.Unlock()
	})
	if s.err != nil {
		m.mu.Lock()
		if m.sessions[id] == s {
			delete(m.sessions, id)
		}
		m.mu.Unlock()
		return nil, s.err
	}
	return s, nil
}

func sameMCP(a, b []MCPServer) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// makeRoomLocked: with MaxSessions running, the one idle longest (not busy, nobody attached) gives way.
func (m *Manager) makeRoomLocked() (*Session, error) {
	live := 0
	var victim *Session
	for _, s := range m.sessions {
		if s.dead() {
			continue
		}
		live++
		in := s.info()
		if in.State == StateIdle && in.Viewers == 0 && (victim == nil || in.LastUsed.Before(victim.info().LastUsed)) {
			victim = s
		}
	}
	if live < m.Config.MaxSessions {
		return nil, nil
	}
	if victim == nil {
		return nil, ErrTooMany
	}
	delete(m.sessions, victim.id)
	return victim, nil
}

// startDriver writes the session's files and starts Claude Code.
func (m *Manager) startDriver(s *Session, resume string) (driver, error) {
	if err := os.MkdirAll(s.cwd, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.ctl, 0o700); err != nil {
		return nil, err
	}
	cfg, env, mcpAllowed := mcpConfig(s.opts.MCP)
	allowed := append(append([]string(nil), m.Config.AllowedTools...), mcpAllowed...)
	mcpFile := filepath.Join(s.ctl, "mcp.json")
	if err := writeJSONFile(mcpFile, cfg); err != nil { // no secrets: ${AIC_MCP_…} references (guard.go)
		return nil, err
	}
	// Claude Code's own files: a json session's in the agents' config folder, with the user's login (home.go)
	home := m.homeEnv(s.opts.Driver)
	if len(home) > 0 {
		if err := os.MkdirAll(m.ClaudeHome(), 0o700); err != nil {
			return nil, err
		}
		_ = os.Chmod(m.ClaudeHome(), 0o700) // the conversations of every session: the worker's user's only
	}
	if err := m.loginError(home); err != nil {
		return nil, err
	}
	s.home, s.env = home, append(append([]string(nil), home...), env...)
	var args []string
	if m.has("--restricted") {
		// Claude Code's own confinement: file tools in the working folder, only the built-in tools named
		tools := strings.Join(sessionTools(m.Config.AllowedTools, len(mcpAllowed) > 0), ",")
		if strings.HasPrefix(s.opts.Key, personKey) {
			tools = "default" // a person at the terminal answers the permission prompts
		}
		args = append(args, "--restricted")
		if tools != "" {
			args = append(args, "--tools", tools)
		}
	}
	args = append(args, "--model", s.opts.Model, "--strict-mcp-config", "--mcp-config", mcpFile,
		"--allowedTools", strings.Join(allowed, ","))
	// the deny rules go into the session's --settings file: they grow with the folders around (other sessions, the
	// config folder) and would not fit on Windows' command line (checkArgs)
	settings := map[string]any{"permissions": map[string]any{"deny": m.denyRules(s)}}
	sys, preamble, err := m.systemArgs(s, resume != "")
	if err != nil {
		return nil, err
	}
	args = append(args, sys...)
	if s.opts.Effort != "" && m.has("--effort") {
		args = append(args, "--effort", s.opts.Effort)
	}
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	if s.opts.Driver == DriverTTY {
		return startTTY(s, args, settings)
	}
	file := filepath.Join(s.ctl, "settings.json")
	if err := writeJSONFile(file, settings); err != nil {
		return nil, err
	}
	return startJSON(s, append([]string{"--settings", file}, args...), preamble)
}

// maxInlineArg: the longest text that goes on the command line itself; longer ones go through a file or stdin.
// Windows' CreateProcess takes 32767 characters for the whole command line (Linux 128 KB for one argument).
const maxInlineArg = 8 << 10

// maxCommandLine: what Windows allows for the whole command line, with room for quoting.
const maxCommandLine = 32000

// systemFile is where the session's system prompt is written for --append-system-prompt-file (in its private
// control folder; removed when the process ends).
func (s *Session) systemFile() string { return filepath.Join(s.ctl, "system.md") }

// hasSystemFile: this claude takes --append-system-prompt-file (a hidden option: --help names it only as
// "--append-system-prompt[-file]").
func (m *Manager) hasSystemFile() bool {
	return m.has("--append-system-prompt-file") || m.has("--append-system-prompt[-file]")
}

// systemArgs are the arguments for the session's system prompt. It never goes on the command line when long (the
// limit above): a file for --append-system-prompt-file, or, for a claude without it, the preamble of the first
// turn's message (json driver; a resumed conversation has it already).
func (m *Manager) systemArgs(s *Session, resumed bool) (args []string, preamble string, err error) {
	file := s.systemFile()
	_ = os.Remove(file)
	sys := s.opts.System
	switch {
	case sys == "":
		return nil, "", nil
	case m.hasSystemFile():
		if err := os.WriteFile(file, []byte(sys), 0o600); err != nil {
			return nil, "", err
		}
		return []string{"--append-system-prompt-file", file}, "", nil
	case len(sys) <= maxInlineArg:
		return []string{"--append-system-prompt", sys}, "", nil
	case s.opts.Driver == DriverTTY:
		return nil, "", fmt.Errorf("the system prompt (%d KB) is too long for this Claude Code's command line: update Claude Code (it takes --append-system-prompt-file)", len(sys)>>10)
	case resumed:
		return nil, "", nil
	}
	return nil, sys, nil
}

// goos is runtime.GOOS (a test sets it).
var goos = runtime.GOOS

// checkArgs: the command line fits (on Windows, where a longer one fails with "The filename or extension is too
// long").
func checkArgs(cmd string, args []string) error {
	n := len(cmd) + 3
	for _, a := range args {
		n += len(a) + 3
	}
	if n > maxCommandLine && goos == "windows" {
		return fmt.Errorf("the command line for claude is %d KB, more than Windows allows (32 KB)", n>>10)
	}
	return nil
}

// mcpName keeps letters, digits, - and _ (Claude Code's tool names are mcp__<server>__<tool>).
func mcpName(n string) string {
	var b strings.Builder
	for _, c := range n {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		}
	}
	return b.String()
}

func writeJSONFile(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// Turn runs one turn in the session of the options (started or reused) and reports its events.
func (m *Manager) Turn(ctx context.Context, o Options, text string, emit func(Event)) (Result, error) {
	o = m.normalize(o)
	if emit == nil {
		emit = func(Event) {}
	}
	s, err := m.open(o, "")
	if err != nil {
		return Result{}, err
	}
	res, err := s.run(ctx, o, s.input(o, text), emit)
	if errors.Is(err, errEndedEarly) && m.forget(s.id) {
		// Claude Code could not resume the conversation (its files are gone): it starts anew — in a new process, not
		// the one that just failed (it may still be ending, and open would hand it out again)
		m.remove(s, false)
		o.Fresh = false
		if s, err = m.open(o, ""); err != nil {
			return Result{}, err
		}
		res, err = s.run(ctx, o, s.input(o, text), emit)
	}
	err = notLoggedIn(err)
	if o.Key == "" {
		m.remove(s, true)
	} else if errors.Is(err, ErrNotLoggedIn) {
		m.remove(s, false) // after the login a new process (resuming the conversation) has it
	}
	return res, err
}

// input is what a turn sends: the Opening while the conversation is new here, else the text.
func (s *Session) input(o Options, text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.Opening != "" && s.turns == 0 && !s.resumed {
		return o.Opening
	}
	return text
}

// forget drops what a conversation would resume; false: there was nothing.
func (m *Manager) forget(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.resume[id]; !ok {
		return false
	}
	delete(m.resume, id)
	_ = writeJSONFile(filepath.Join(m.Root, "resume.json"), m.resume)
	return true
}

func (s *Session) run(ctx context.Context, o Options, text string, emit func(Event)) (Result, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Result{}, errors.New("the session was closed")
	}
	s.busy = true
	drv := s.drv
	s.mu.Unlock()
	res, err := turnWith(ctx, drv, text, turnImages(o, text), emit)
	res.Session, res.Driver = s.id, s.opts.Driver
	if err == nil {
		s.m.loggedIn(s.home) // the next session start need not ask
	}
	s.mu.Lock()
	s.busy, s.lastUsed = false, time.Now()
	s.turns++
	s.mu.Unlock()
	if cs := drv.claudeSession(); cs != "" && s.opts.Key != "" {
		s.m.remember(s.id, cs)
	}
	select {
	case <-drv.exited(): // the process ended: the next turn starts a new one (with --resume)
		s.m.remove(s, false)
	default:
	}
	return res, err
}

// remember keeps Claude Code's session id of a conversation (resume.json), so it continues after a restart.
func (m *Manager) remember(id, claude string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resume[id] == claude {
		return
	}
	m.resume[id] = claude
	_ = os.MkdirAll(m.Root, 0o755)
	_ = writeJSONFile(filepath.Join(m.Root, "resume.json"), m.resume)
}

// shut ends the process.
func (s *Session) shut() {
	s.mu.Lock()
	s.closed = true
	drv := s.drv
	s.mu.Unlock()
	if drv != nil {
		drv.close()
	}
	_ = os.Remove(s.systemFile()) // read when the process started; written anew for the next one
}

// remove ends a session; a one-off one also loses its folders.
func (m *Manager) remove(s *Session, oneOff bool) {
	m.mu.Lock()
	if m.sessions[s.id] == s {
		delete(m.sessions, s.id)
	}
	if oneOff {
		delete(m.resume, s.id)
	}
	m.mu.Unlock()
	s.shut()
	if oneOff {
		_ = os.RemoveAll(s.ctl)
		if s.opts.Cwd == "" {
			for _, p := range m.projectDirs(s.cwd) { // its transcript: else a deny rule of every later session
				_ = os.RemoveAll(p)
			}
			_ = os.RemoveAll(s.cwd)
		}
	}
}

// List is every session, the newest first.
func (m *Manager) List() []Info {
	m.mu.Lock()
	m.init()
	ss := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		ss = append(ss, s)
	}
	m.mu.Unlock()
	out := make([]Info, 0, len(ss))
	for _, s := range ss {
		if !s.dead() {
			out = append(out, s.info())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// Get is one session's info.
func (m *Manager) Get(id string) (Info, bool) {
	m.mu.Lock()
	m.init()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil || s.dead() {
		return Info{}, false
	}
	return s.info(), true
}

// Stop ends a session's process (a later turn of its conversation starts a new one, which resumes it).
func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	m.init()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return ErrNoSession
	}
	m.remove(s, false)
	return nil
}

// OpenTTY starts an interactive session for a person (POST /api/agents/sessions): a tty session of its own,
// closed after the idle timeout like the others (never while someone is attached).
func (m *Manager) OpenTTY(owner, model, cwd string) (Info, error) {
	if cwd != "" {
		real, err := m.CheckCwd(cwd)
		if err != nil {
			return Info{}, err
		}
		cwd = real
	}
	b := make([]byte, 6)
	rand.Read(b)
	id := "p" + hex.EncodeToString(b)
	o := m.normalize(Options{Key: personKey + id, Owner: owner, Model: model, Driver: DriverTTY, Cwd: cwd})
	s, err := m.open(o, id)
	if err != nil {
		return Info{}, err
	}
	return s.info(), nil
}

// personKey starts the conversation key of a person's own tty session.
const personKey = "person:"

// Terminal is an attached view of a tty session.
type Terminal interface {
	// Attach gives what the terminal showed so far, then its output as it comes (closed when the session ends or
	// this viewer falls behind); detach ends it.
	Attach() (snapshot []byte, out <-chan []byte, detach func())
	Write(p []byte) error
	Resize(cols, rows int) error
}

// Terminal is the terminal of a tty session.
func (m *Manager) Terminal(id string) (Terminal, error) {
	m.mu.Lock()
	m.init()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil || s.dead() {
		return nil, ErrNoSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.drv.(*ttyDriver)
	if !ok {
		return nil, ErrNotTTY
	}
	return t, nil
}

// Run closes idle sessions until ctx ends, then every session.
func (m *Manager) Run(ctx context.Context) {
	every := m.reapEvery
	if every == 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.Close()
			return
		case <-t.C:
			m.Reap(time.Now())
		}
	}
}

// Reap closes the sessions idle longer than the timeout (not busy, nobody attached).
func (m *Manager) Reap(now time.Time) int {
	m.mu.Lock()
	m.init()
	var gone []*Session
	for _, s := range m.sessions {
		in := s.info()
		if s.dead() || in.State == StateIdle && in.Viewers == 0 && now.Sub(in.LastUsed) >= m.Config.Idle() {
			gone = append(gone, s)
			delete(m.sessions, s.id)
		}
	}
	m.mu.Unlock()
	for _, s := range gone {
		s.shut()
	}
	return len(gone)
}

// Close ends every session (the worker stops).
func (m *Manager) Close() {
	m.mu.Lock()
	m.init()
	all := make([]*Session, 0, len(m.sessions))
	for id, s := range m.sessions {
		all = append(all, s)
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.shut()
	}
}

// environ is the environment of claude.
func (m *Manager) environ(extra ...string) []string {
	env := os.Environ()
	env = append(env, m.Env...)
	return append(env, extra...)
}

// shorten cuts s to n runes.
func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
