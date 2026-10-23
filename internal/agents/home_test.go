package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// unsetEnv removes variables of the test's environment for the test (restored after it).
func unsetEnv(t *testing.T, keys ...string) {
	for _, k := range keys {
		t.Setenv(k, "") // registers the restore
		os.Unsetenv(k)
	}
}

func lacks(rules []string, rule string) bool { return !slices.Contains(rules, rule) }

// A json session keeps Claude Code's own files in the agents' config folder (.claude-home), with the user's login:
// it may read its own project folder there (the large tool results Claude Code saves for it), nothing else of it,
// and still nothing of the user's ~/.claude*; a tty session uses the user's config and nothing of .claude-home.
func TestClaudeHome(t *testing.T) {
	unsetEnv(t, "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS")
	m, argsLog := guardedManager(t)
	home := filepath.Join(m.Root, ".claude-home")
	id := sessionID("alice", "board-9")
	own, whole := projectName(filepath.Join(m.Root, id))
	if !whole || strings.ContainsAny(own, "/\\._") {
		t.Fatalf("project name %q", own)
	}
	other, _ := projectName(filepath.Join(m.Root, sessionID("bob", "board-1")))
	for _, d := range []string{filepath.Join(m.Root, sessionID("bob", "board-1")), // a session there now (pruneProjects)
		filepath.Join(home, "projects", own, "sid-1", "tool-results"), filepath.Join(home, "projects", other),
		filepath.Join(home, "backups")} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{}}`), 0o600)
	os.WriteFile(filepath.Join(home, "custom.json"), []byte(`{}`), 0o600)

	o := Options{Key: "board-9", Owner: "alice", MCP: []MCPServer{{Name: "vboard", URL: "http://x/mcp"}}}
	if _, err := m.Turn(context.Background(), o, "hi", nil); err != nil {
		t.Fatal(err)
	}
	all := startsOf(t, argsLog)
	s := all[len(all)-1]
	want := map[string]string{"CLAUDE_CONFIG_DIR": home, "CLAUDE_SECURESTORAGE_CONFIG_DIR": "", "CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS": "60000"}
	if fmt.Sprint(s.ClaudeEnv) != fmt.Sprint(want) {
		t.Fatalf("claude's environment %v, want %v", s.ClaudeEnv, want)
	}
	if st, err := os.Stat(home); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o700 {
		t.Fatalf(".claude-home %v %v", st, err)
	}
	denyFlag, _ := s.denied()
	deny := strings.Split(denyFlag, ",")
	for _, p := range []string{".claude.json", "custom.json", "backups", "settings.json", ".credentials.json", filepath.Join("projects", other)} {
		r := absRule(filepath.Join(home, p))
		if lacks(deny, "Read("+r+")") || lacks(deny, "Read("+r+"/**)") || lacks(deny, "Grep("+r+"/**)") || lacks(deny, "Edit("+r+")") {
			t.Errorf("%s not denied: %s", p, denyFlag)
		}
	}
	if lacks(deny, "Read("+absRule(filepath.Join(home, ".claude.json"))+"*)") {
		t.Errorf(".claude.json* not denied")
	}
	// the user's own Claude Code files: all of them, as before
	for _, r := range []string{"Read(~/.claude*)", "Read(~/.claude*/**)", "Glob(~/.claude*/**)", "Grep(~/.claude*/**)", "Read(~/.ssh/**)"} {
		if lacks(deny, r) {
			t.Errorf("%s missing", r)
		}
	}
	// never the session's own project folder (a deny would beat --restricted's allowance of its saved results)
	for _, p := range []string{home, filepath.Join(home, "projects"), filepath.Join(home, "projects", own)} {
		if strings.Contains(denyFlag, absRule(p)+")") || strings.Contains(denyFlag, absRule(p)+"/**)") {
			t.Errorf("%s is denied", p)
		}
	}

	// a tty session: the user's config as it is, nothing of the agents' one
	if runtime.GOOS != "windows" {
		if _, err := m.OpenTTY("alice", "", ""); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, func() bool { all = startsOf(t, argsLog); return len(all) == 2 })
		tty := all[1]
		if len(tty.ClaudeEnv) != 0 {
			t.Fatalf("tty environment %v", tty.ClaudeEnv)
		}
		d, _ := tty.denied()
		if !strings.Contains(","+d+",", ",Read("+absRule(home)+"/**),") || !strings.Contains(d, "Read(~/.claude*/**)") {
			t.Fatalf("tty deny %s", d)
		}
	}
	if _, err := m.CheckCwd(filepath.Join(home, "projects")); err == nil {
		t.Fatal("a tty session may work in .claude-home")
	}
}

// A user with a Claude Code config folder of their own (CLAUDE_CONFIG_DIR): its login is used and it is denied;
// what the worker's environment sets is kept.
func TestClaudeHomeUserConfigDir(t *testing.T) {
	unsetEnv(t, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
	userDir := filepath.Join(t.TempDir(), "my-claude")
	t.Setenv("CLAUDE_CONFIG_DIR", userDir)
	t.Setenv("CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS", "30000")
	m, argsLog := guardedManager(t)
	if _, err := m.Turn(context.Background(), Options{Key: "k", Owner: "alice"}, "hi", nil); err != nil {
		t.Fatal(err)
	}
	s := startsOf(t, argsLog)[0]
	if s.ClaudeEnv["CLAUDE_CONFIG_DIR"] != filepath.Join(m.Root, ".claude-home") || s.ClaudeEnv["CLAUDE_SECURESTORAGE_CONFIG_DIR"] != userDir ||
		s.ClaudeEnv["CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS"] != "30000" {
		t.Fatalf("environment %v", s.ClaudeEnv)
	}
	if d, _ := s.denied(); !strings.Contains(d, "Read("+absRule(userDir)+"/**)") {
		t.Fatalf("the user's config folder is not denied: %s", d)
	}
}

// agents.json claudeConfig "user": the sessions use the user's Claude Code config (the old way), and a .claude-home
// left from before is denied entirely.
func TestClaudeConfigUser(t *testing.T) {
	unsetEnv(t, "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS")
	m, argsLog := guardedManager(t)
	m.Config.ClaudeConfig = ClaudeConfigUser
	os.MkdirAll(filepath.Join(m.Root, ".claude-home", "projects"), 0o700)
	if _, err := m.Turn(context.Background(), Options{Key: "k", Owner: "alice"}, "hi", nil); err != nil {
		t.Fatal(err)
	}
	s := startsOf(t, argsLog)[0]
	if len(s.ClaudeEnv) != 0 {
		t.Fatalf("environment %v", s.ClaudeEnv)
	}
	if d, _ := s.denied(); !strings.Contains(d, "Read("+absRule(filepath.Join(m.Root, ".claude-home"))+"/**)") {
		t.Fatalf(".claude-home not denied: %s", d)
	}
}

func TestProjectName(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/a/.config/mlxlab/agents/s0123456789ab": "-Users-a--config-mlxlab-agents-s0123456789ab",
		`C:\Users\a b\agents\x`:                        "C--Users-a-b-agents-x",
		"/tmp/ä/😀":                                     "-tmp-----",
	} {
		if got, whole := projectName(in); got != want || !whole {
			t.Errorf("projectName(%q) = %q, %v; want %q", in, got, whole, want)
		}
	}
	if got, whole := projectName("/" + strings.Repeat("a", 300)); whole || len(got) != 200 {
		t.Errorf("long: %d %v", len(got), whole)
	}
}

// Without a login a session does not start: the job fails with what to do. A Claude Code that cannot say so
// beforehand fails the turn with its own words, which are taken the same way.
func TestNotLoggedIn(t *testing.T) {
	for _, silent := range []bool{false, true} {
		m, argsLog := guardedManager(t)
		m.Env = append(m.Env, "AGENTS_FAKE_LOGGED_OUT=1")
		if silent {
			m.Env = append(m.Env, "AGENTS_FAKE_AUTH_SILENT=1")
		}
		_, err := m.Turn(context.Background(), Options{Key: "k", Owner: "alice"}, "hi", nil)
		if !errors.Is(err, ErrNotLoggedIn) || !strings.Contains(err.Error(), "ai-connector agents login") {
			t.Fatalf("silent=%v: %v", silent, err)
		}
		if n := len(startsOf(t, argsLog)); silent != (n > 0) {
			t.Fatalf("silent=%v: %d starts", silent, n)
		}
	}
}

// `ai-connector agents login|status` run claude auth as the json sessions do: their config folder, the user's login.
func TestAuthCommand(t *testing.T) {
	unsetEnv(t, "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CLAUDE_CODE_FILE_READ_MAX_OUTPUT_TOKENS")
	m, _ := guardedManager(t)
	log := filepath.Join(t.TempDir(), "auth.log")
	m.Env = append(m.Env, "AGENTS_FAKE_AUTH_LOG="+log)
	out, err := m.AuthCommand("status", "--json").Output()
	if err != nil || !strings.Contains(string(out), `"loggedIn":true`) {
		t.Fatalf("%s %v", out, err)
	}
	b, _ := os.ReadFile(log)
	var env map[string]string
	if json.Unmarshal(b, &env) != nil || env["CLAUDE_CONFIG_DIR"] != filepath.Join(m.Root, ".claude-home") {
		t.Fatalf("auth environment %s", b)
	}
	if v, ok := env["CLAUDE_SECURESTORAGE_CONFIG_DIR"]; !ok || v != "" {
		t.Fatalf("auth environment %s", b)
	}
}

// The project folders of session folders that are gone are removed when the manager starts (each was a deny rule
// of every later session, until claude's arguments passed the system's limit); those of folders there now, and
// those of folders outside the agents folder, stay. A one-off session takes its project folder along.
func TestPruneProjects(t *testing.T) {
	unsetEnv(t, "CLAUDE_CONFIG_DIR")
	root := t.TempDir()
	projects := filepath.Join(root, ".claude-home", "projects")
	name := func(dir string) string { n, _ := projectName(dir); return n }
	os.MkdirAll(filepath.Join(root, "o-live"), 0o755)
	keep := []string{name(filepath.Join(root, "o-live")), name(filepath.Join(root, "o-live", "sub")), name("/Users/someone/dev/app")}
	gone := []string{name(filepath.Join(root, "o-gone")), name(filepath.Join(root, "o-gone2"))}
	for _, n := range append(append([]string(nil), keep...), gone...) {
		os.MkdirAll(filepath.Join(projects, n, "sid"), 0o755)
	}
	m := &Manager{Root: root}
	m.init()
	for _, n := range keep {
		if _, err := os.Stat(filepath.Join(projects, n)); err != nil {
			t.Errorf("%s removed", n)
		}
	}
	for _, n := range gone {
		if _, err := os.Stat(filepath.Join(projects, n)); err == nil {
			t.Errorf("%s kept", n)
		}
	}

	s := &Session{id: "o-live", cwd: filepath.Join(root, "o-live"), ctl: filepath.Join(root, ".ctl", "o-live")}
	m.sessions[s.id] = s
	m.remove(s, true)
	if _, err := os.Stat(filepath.Join(projects, keep[0])); err == nil {
		t.Errorf("a one-off session's project folder stayed")
	}
	if _, err := os.Stat(filepath.Join(projects, keep[2])); err != nil {
		t.Errorf("a folder outside the agents folder went")
	}
}
