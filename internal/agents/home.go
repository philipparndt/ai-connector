package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Claude Code's own files (docs/api.md, "Agents (Claude Code)", "Claude Code's own files").
//
// Claude Code keeps a session's transcript, and every tool result larger than it puts into the conversation, under
// its config folder: <config>/projects/<the working folder as a name>/<its session id>/tool-results/<call>.json.
// The model gets "Output too large (…). Full output saved to: <path>" plus a 2 KB preview, and is meant to Read the
// file. Measured with Claude Code 2.1.288: an MCP tool's result is saved this way above 50 000 characters
// (min(maxResultSizeChars 100 000, a 50 000 ceiling); only a server's tool _meta "anthropic/maxResultSizeChars"
// raises it, up to 500 000). MAX_MCP_OUTPUT_TOKENS (default 25 000) is another limit: it cuts results down before
// they are saved, it does not keep them in the conversation.
//
// The sessions may not read the user's ~/.claude* (credentials, settings, the user's own conversations), and deny
// rules always beat allow rules, so a result saved there was out of the model's reach. A json session therefore
// runs with a config folder of the agents' own (CLAUDE_CONFIG_DIR=<agents>/.claude-home): the session may read
// its own saved results there (--restricted lets the file tools reach exactly those outside the working folder),
// while ~/.claude* stays denied entirely, and so does everything in .claude-home but the session's own project
// folder. The login stays the user's: CLAUDE_SECURESTORAGE_CONFIG_DIR names where Claude Code finds its
// credentials ("" = the default: the keychain item "Claude Code-credentials" on macOS, ~/.claude/.credentials.json
// elsewhere), so nothing is copied and no second login is needed.

// claudeHomeName is the agents' own Claude Code config folder inside the agents folder.
const claudeHomeName = ".claude-home"

// fileReadTokens is the Read tool's limit for the json sessions (CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS, default
// 25 000): a saved MCP result is a JSON array with the whole text on one line, which only a Read of the whole file
// gets (Read pages by lines, Grep leaves out lines over 500 characters). The largest MCP result Claude Code keeps
// (MAX_MCP_OUTPUT_TOKENS, 25 000 tokens) is about 30 000 tokens as JSON.
const fileReadTokens = "60000"

// ClaudeHome is the agents' own Claude Code config folder ("" when the json sessions use the user's: agents.json
// claudeConfig "user").
func (m *Manager) ClaudeHome() string {
	if m.Config.ClaudeConfig == ClaudeConfigUser {
		return ""
	}
	return filepath.Join(m.Root, claudeHomeName)
}

// lookupEnv is a variable of claude's environment (the manager's Env, else the worker's own).
func (m *Manager) lookupEnv(k string) (string, bool) {
	for i := len(m.Env) - 1; i >= 0; i-- {
		if key, v, ok := strings.Cut(m.Env[i], "="); ok && key == k {
			return v, true
		}
	}
	return os.LookupEnv(k)
}

// homeEnv is what a session's claude gets in its environment for Claude Code's own files: for a json session the
// agents' config folder, the user's login, and the larger Read limit; nothing for a tty session (a person may
// attach: it uses the user's Claude Code as it is, whose first-run questions a new config folder would ask again).
func (m *Manager) homeEnv(driver string) []string {
	home := m.ClaudeHome()
	if home == "" || driver == DriverTTY {
		return nil
	}
	env := []string{"CLAUDE_CONFIG_DIR=" + home}
	if _, ok := m.lookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR"); !ok {
		// the credentials of the user's config folder: their CLAUDE_CONFIG_DIR if they set one, else the default
		user, _ := m.lookupEnv("CLAUDE_CONFIG_DIR")
		env = append(env, "CLAUDE_SECURESTORAGE_CONFIG_DIR="+user)
	}
	if _, ok := m.lookupEnv("CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS"); !ok {
		env = append(env, "CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS="+fileReadTokens)
	}
	return env
}

// projectName is Claude Code's folder name for a working folder (<config>/projects/<name>): every character but
// ASCII letters and digits becomes "-" (per UTF-16 unit, as JavaScript counts them); a name over 200 characters
// is cut there and gets a hash (which this does not compute: the second result is then false).
func projectName(dir string) (string, bool) {
	var b strings.Builder
	for _, r := range dir {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	n := b.String()
	if len(n) > 200 {
		return n[:200], false
	}
	return n, true
}

// homeDenyPaths are the paths of the agents' config folder a json session may not read: everything but its own
// project folder (its transcript and saved tool results) — the other sessions' project folders and Claude Code's
// files (.claude.json with the account, settings, backups, …). Entries made later are not known here; --restricted
// keeps the file tools from them (it lets them reach only the session's own saved results outside its folder).
func (m *Manager) homeDenyPaths(s *Session) (exact, prefix []string) {
	home := m.ClaudeHome()
	if home == "" {
		return nil, nil
	}
	if s.opts.Driver == DriverTTY {
		return []string{home}, nil // a tty session uses the user's config folder: nothing of this one
	}
	known := []string{".claude.json", ".credentials.json", "settings.json", "settings.local.json", "backups", "sessions",
		"session-env", "shell-snapshots", "todos", "debug", "statsig", "plugins", "file-history", "history.jsonl", "cache",
		"ide", "paste-cache", "jobs", "state", "daemon", "remote-settings.json", "policy-limits.json", "stats-cache.json"}
	if es, err := os.ReadDir(home); err == nil {
		for _, e := range es {
			known = append(known, e.Name())
		}
	}
	seen := map[string]bool{"projects": true}
	for _, n := range known {
		if !seen[n] {
			seen[n] = true
			exact = append(exact, filepath.Join(home, n))
		}
	}
	// .claude.json.backup, .claude.json.lock, … next to .claude.json
	prefix = append(prefix, filepath.Join(home, ".claude.json"))
	own := map[string]bool{}
	cut := []string{}
	for _, d := range []string{s.cwd, mustReal(s.cwd)} {
		if n, whole := projectName(d); whole {
			own[n] = true
		} else {
			cut = append(cut, n)
		}
	}
	projects := filepath.Join(home, "projects")
	if es, err := os.ReadDir(projects); err == nil {
		for _, e := range es {
			n := e.Name()
			mine := own[n]
			for _, c := range cut {
				mine = mine || strings.HasPrefix(n, c)
			}
			if !mine {
				exact = append(exact, filepath.Join(projects, n))
			}
		}
	}
	return exact, prefix
}

// ErrNotLoggedIn: Claude Code has no login for the agents here.
var ErrNotLoggedIn = errors.New("Claude Code is not logged in on this worker: log in once on it with " +
	"`ai-connector agents login` (or `claude auth login`), then try again")

// How long a login is trusted: one seen (`claude auth status`, or a turn that worked) for ten minutes, none for one
// (asking takes ~0.5 s with Claude Code 2.1.288, so a session start in use does not ask again).
var (
	loginTTL   = 10 * time.Minute
	noLoginTTL = time.Minute
)

// loginError says whether claude, started with env, has a login (`claude auth status --json`): ErrNotLoggedIn when
// it says it has none; nil when it has one or cannot say (an older Claude Code).
func (m *Manager) loginError(env []string) error {
	key := strings.Join(env, "\x00")
	m.probeMu.Lock()
	if c, ok := m.logins[key]; ok && (c.err == nil && time.Since(c.at) < loginTTL || c.err != nil && time.Since(c.at) < noLoginTTL) {
		m.probeMu.Unlock()
		return c.err
	}
	m.probeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.command(), "auth", "status", "--json")
	cmd.Env = m.environ(env...)
	out, _ := cmd.Output() // it exits with 1 when logged out: the JSON says so
	var st struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	var res error // nil also when it cannot say (no JSON): the turn will tell
	if json.Unmarshal(out, &st) == nil && st.LoggedIn != nil && !*st.LoggedIn {
		res = ErrNotLoggedIn
	}
	m.probeMu.Lock()
	if m.logins == nil {
		m.logins = map[string]loginState{}
	}
	m.logins[key] = loginState{at: time.Now(), err: res}
	m.probeMu.Unlock()
	return res
}

type loginState struct {
	at  time.Time
	err error
}

// loggedIn notes that claude with env has a login (a turn worked).
func (m *Manager) loggedIn(env []string) {
	m.probeMu.Lock()
	defer m.probeMu.Unlock()
	if m.logins == nil {
		m.logins = map[string]loginState{}
	}
	m.logins[strings.Join(env, "\x00")] = loginState{at: time.Now()}
}

// notLoggedIn maps Claude Code's "Not logged in · Please run /login" (a turn's error) to ErrNotLoggedIn.
func notLoggedIn(err error) error {
	if err != nil && strings.Contains(err.Error(), "Not logged in") {
		return fmt.Errorf("%w (%s)", ErrNotLoggedIn, shorten(err.Error(), 200))
	}
	return err
}

// AuthCommand is `claude auth <args>` as the json sessions run claude (their config folder and login), for
// `ai-connector agents login|status`.
func (m *Manager) AuthCommand(args ...string) *exec.Cmd {
	m.mu.Lock()
	m.init()
	m.mu.Unlock()
	cmd := exec.Command(m.command(), append([]string{"auth"}, args...)...)
	if home := m.ClaudeHome(); home != "" {
		_ = os.MkdirAll(home, 0o700)
	}
	cmd.Env = m.environ(m.homeEnv(DriverJSON)...)
	return cmd
}

// projectDirs are the project folders of a working folder in the agents' config folder (by its path and its
// resolved path; nil when the sessions use the user's config folder).
func (m *Manager) projectDirs(cwd string) []string {
	home := m.ClaudeHome()
	if home == "" {
		return nil
	}
	var dirs []string
	for _, d := range []string{cwd, mustReal(cwd)} {
		if n, whole := projectName(d); whole {
			p := filepath.Join(home, "projects", n)
			if len(dirs) == 0 || dirs[0] != p {
				dirs = append(dirs, p)
			}
		}
	}
	return dirs
}

// pruneProjects removes the project folders of session folders that are gone: a one-off session's folders went,
// its project folder stayed (before remove took it along). Each one left behind was another deny rule of every
// session started later (homeDenyPaths), until the arguments of claude passed the system's limit ("argument list
// too long"). Only the names of folders inside the agents folder are looked at; a project of a folder there now
// (or below it) stays.
func (m *Manager) pruneProjects() {
	home := m.ClaudeHome()
	if home == "" {
		return
	}
	es, err := os.ReadDir(filepath.Join(home, "projects"))
	if err != nil {
		return
	}
	var roots, keep []string
	for _, r := range []string{m.Root, mustReal(m.Root)} {
		if n, whole := projectName(r); whole {
			roots = append(roots, n+"-")
		}
	}
	if sessions, err := os.ReadDir(m.Root); err == nil {
		for _, s := range sessions {
			for _, r := range []string{m.Root, mustReal(m.Root)} {
				if n, whole := projectName(filepath.Join(r, s.Name())); whole {
					keep = append(keep, n)
				}
			}
		}
	}
	for _, e := range es {
		n := e.Name()
		under := false
		for _, r := range roots {
			under = under || strings.HasPrefix(n, r)
		}
		for _, k := range keep {
			under = under && n != k && !strings.HasPrefix(n, k+"-")
		}
		if under {
			_ = os.RemoveAll(filepath.Join(home, "projects", n))
		}
	}
}
