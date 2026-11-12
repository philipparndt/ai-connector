package main

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// `ai-connector vboard --install` starts it at login: a launchd agent on macOS, a systemd user unit on Linux, a
// scheduled task on Windows (the Run key of the user when Windows refuses the task). It runs `ai-connector vboard`
// without flags — the settings, the token too, are in <files>/vboard/settings.json (0600), not in the service's
// file (on Windows the task's command carries --env for where the files are). A copy that runs already with these
// settings (from a terminal, the service's older one) is stopped: the service takes over (instance.go). `--uninstall`
// stops and removes it.
//
// `ai-connector worker --install` the same way under names of its own (workerKind): hub and token stay in worker.json
// (or --config's file, 0600), the service's command has only --config and --state (paths) when they were given.

// serviceKind is what is started: its names on each system and its command after the program.
type serviceKind struct {
	label string   // launchd
	unit  string   // systemd --user
	task  string   // schtasks, the Run key's value
	log   string   // launchd's log, in ~/Library/Logs
	what  string   // the command, as people type it (messages)
	desc  string   // the systemd unit's Description
	args  []string // after the program: the mode first, then its flags
}

var vboardKind = serviceKind{label: "de.rnd7.ai-connector.vboard", unit: "ai-connector-vboard.service", task: "ai-connector vboard",
	log: "ai-connector-vboard.log", what: "ai-connector vboard", desc: "ai-connector for vboard (vboard's local AI)", args: []string{"vboard"}}

const (
	serviceRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	schtasksMaxTR = 261 // schtasks /TR refuses a longer command; the Run key has no such limit
)

// serviceEnvKeys are kept from the environment of `--install` (where the files and the models are, what the agents
// find on the PATH).
var serviceEnvKeys = []string{"PATH", "MLXLAB_HOME", "XDG_CONFIG_HOME", "MLXLAB_STATE", "MLXLAB_MODELS_FROM", "MLXLAB_RUNTIMES",
	"MLXLAB_GPU", "MLXLAB_IDLE_UNLOAD", "MLXLAB_PIN", "MLXLAB_ALLOW_LOCAL_RUNTIMES",
	"HF_HOME", "HF_HUB_CACHE", "HF_TOKEN_PATH", "CLAUDE_CONFIG_DIR"}

// service is the start at login on one system.
type service struct {
	kind *serviceKind // nil: vboardKind
	goos string
	home string
	exe  string
	env  [][2]string // KEY, value
	uid  int
	// run runs a command; it returns its output (tests record the commands instead)
	run func(name string, args ...string) (string, error)
	// holder is the PID of the running `ai-connector vboard` that holds the settings folder's lock (held false: none,
	// whatever PID the file has); stop ends that process, then install waits up to stopWait for the lock to be free.
	// Tests replace them.
	holder   func() (pid int, held bool)
	stop     func(pid int) error
	stopWait time.Duration
	// bootWait: how long install keeps trying `launchctl bootstrap`, a try every bootPoll (macOS: an earlier service
	// that is still ending makes it fail)
	bootWait, bootPoll time.Duration
	out                io.Writer // what install says besides describe (nil: stdout)
	runKey             bool      // Windows: installed in the Run key, not as a task
	// startNow starts `exe args…` in the background, apart from this console (Windows: when only the Run key could be
	// set, which starts it at the next login — "now and at every login" starts it now this way). Tests record it.
	startNow func(exe string, args []string) error
}

func (s service) k() *serviceKind {
	if s.kind == nil {
		return &vboardKind
	}
	return s.kind
}

// newService is kind's service on this system; lock is the instance lock of the copy it replaces (instance.go).
func newService(getenv func(string) string, kind serviceKind, lock string) (service, error) {
	exe, err := os.Executable()
	if err != nil {
		return service{}, err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if strings.Contains(exe, "go-build") {
		return service{}, fmt.Errorf("%s is a temporary build (go run): build ai-connector and install from the program", exe)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return service{}, err
	}
	s := service{kind: &kind, goos: runtime.GOOS, home: home, exe: exe, uid: os.Getuid(), run: func(name string, args ...string) (string, error) {
		out, err := exec.Command(name, args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}, holder: func() (int, bool) { return lockHolder(lock) }, stop: stopProcess, stopWait: 10 * time.Second, bootWait: 45 * time.Second, bootPoll: time.Second, startNow: startDetached}
	for _, k := range serviceEnvKeys {
		if v := getenv(k); v != "" {
			s.env = append(s.env, [2]string{k, v})
		}
	}
	return s, nil
}

// file is where the service is described ("" on Windows: the task holds it).
func (s service) file() string {
	switch s.goos {
	case "darwin":
		return filepath.Join(s.home, "Library", "LaunchAgents", s.k().label+".plist")
	case "linux":
		cfg := filepath.Join(s.home, ".config")
		for _, kv := range s.env {
			if kv[0] == "XDG_CONFIG_HOME" {
				cfg = kv[1]
			}
		}
		return filepath.Join(cfg, "systemd", "user", s.k().unit)
	}
	return ""
}

// logFile is where a launchd agent writes ("" elsewhere: journalctl --user -u ai-connector-vboard on Linux).
func (s service) logFile() string {
	if s.goos == "darwin" {
		return filepath.Join(s.home, "Library", "Logs", s.k().log)
	}
	return ""
}

func xmlEsc(v string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(v))
	return b.String()
}

// systemdQuote quotes a word for ExecStart= and Environment= ("…" with \ and " escaped, % doubled).
func systemdQuote(v string) string {
	v = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(v)
	return `"` + v + `"`
}

// content is the launchd plist or the systemd unit.
func (s service) content() string {
	var b strings.Builder
	switch s.goos {
	case "darwin":
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + xmlEsc(s.k().label) + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEsc(s.exe) + `</string>
`)
		for _, a := range s.k().args {
			b.WriteString("\t\t<string>" + xmlEsc(a) + "</string>\n")
		}
		b.WriteString(`	</array>
`)
		if len(s.env) > 0 {
			b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
			for _, kv := range s.env {
				b.WriteString("\t\t<key>" + xmlEsc(kv[0]) + "</key>\n\t\t<string>" + xmlEsc(kv[1]) + "</string>\n")
			}
			b.WriteString("\t</dict>\n")
		}
		b.WriteString(`	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardOutPath</key>
	<string>` + xmlEsc(s.logFile()) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEsc(s.logFile()) + `</string>
</dict>
</plist>
`)
	case "linux":
		b.WriteString("[Unit]\nDescription=" + s.k().desc + "\nWants=network-online.target\nAfter=network-online.target\n\n[Service]\n")
		b.WriteString("ExecStart=" + systemdQuote(s.exe))
		for i, a := range s.k().args {
			if i > 0 { // the mode as it is, its flags quoted
				a = systemdQuote(a)
			}
			b.WriteString(" " + a)
		}
		b.WriteString("\n")
		for _, kv := range s.env {
			b.WriteString("Environment=" + systemdQuote(kv[0]+"="+kv[1]) + "\n")
		}
		b.WriteString("Restart=always\nRestartSec=10\n\n[Install]\nWantedBy=default.target\n")
	}
	return b.String()
}

// commandArgs are command's words after the program, unquoted (for starting it from here).
func (s service) commandArgs() []string {
	args := slices.Clone(s.k().args)
	for _, kv := range s.env {
		if kv[0] != "PATH" {
			args = append(args, "--env", kv[0]+"="+kv[1])
		}
	}
	return args
}

// command is the Windows task's command line. A task or the Run key starts it with the user's environment, not the
// one of `--install`: what was kept from that comes as --env (PATH not: the user's own is there).
func (s service) command() string {
	parts := []string{`"` + s.exe + `"`}
	for _, a := range s.k().args {
		parts = append(parts, winQuote(a))
	}
	for _, kv := range s.env {
		if kv[0] != "PATH" {
			parts = append(parts, "--env", winQuote(kv[0]+"="+kv[1]))
		}
	}
	return strings.Join(parts, " ")
}

// winQuote quotes a word for a Windows command line (CommandLineToArgvW: "…"; a " as \", the \ before it doubled).
func winQuote(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\"") {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(v[i])
	}
	b.WriteString(strings.Repeat(`\`, slashes)) // before the closing "
	b.WriteByte('"')
	return b.String()
}

// installCmds start the service (after its file is written).
func (s service) installCmds() [][]string {
	switch s.goos {
	case "darwin":
		domain := fmt.Sprintf("gui/%d", s.uid)
		return [][]string{{"launchctl", "bootout", domain + "/" + s.k().label}, {"launchctl", "bootstrap", domain, s.file()}}
	case "linux":
		return [][]string{{"systemctl", "--user", "daemon-reload"}, {"systemctl", "--user", "enable", s.k().unit},
			{"systemctl", "--user", "restart", s.k().unit}}
	case "windows":
		return [][]string{{"schtasks", "/Create", "/TN", s.k().task, "/TR", s.command(), "/SC", "ONLOGON", "/RL", "LIMITED", "/F"},
			{"schtasks", "/Run", "/TN", s.k().task}}
	}
	return nil
}

// uninstallCmds stop and remove it (their failures do not count: it may not be there).
func (s service) uninstallCmds() [][]string {
	switch s.goos {
	case "darwin":
		return [][]string{{"launchctl", "bootout", fmt.Sprintf("gui/%d/%s", s.uid, s.k().label)}}
	case "linux":
		return [][]string{{"systemctl", "--user", "disable", "--now", s.k().unit}}
	case "windows":
		return [][]string{{"schtasks", "/End", "/TN", s.k().task}, {"schtasks", "/Delete", "/TN", s.k().task, "/F"},
			{"reg", "delete", serviceRunKey, "/v", s.k().task, "/f"}}
	}
	return nil
}

func (s *service) printf(format string, a ...any) {
	w := s.out
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintf(w, format, a...)
}

// replaceRunning stops the copy of `ai-connector vboard` (or worker) that runs with these settings, if one does: only the process
// whose PID is in the lock file while it holds the lock. Not stopping it is no error: the service waits for it.
func (s *service) replaceRunning() {
	if s.holder == nil {
		return
	}
	pid, held := s.holder()
	if !held {
		return
	}
	if pid <= 0 {
		s.printf("another %s runs (its PID is unknown): the service waits until it ends\n", s.k().what)
		return
	}
	if err := s.stop(pid); err != nil {
		s.printf("could not stop the running copy (pid %d: %v): the service waits until it ends\n", pid, err)
		return
	}
	for end := time.Now().Add(s.stopWait); ; time.Sleep(100 * time.Millisecond) {
		if _, held := s.holder(); !held {
			s.printf("stopped the running copy (pid %d): the service takes over\n", pid)
			return
		}
		if time.Now().After(end) {
			s.printf("the running copy (pid %d) did not end within %s: the service waits until it does\n", pid, s.stopWait)
			return
		}
	}
}

// startedNow follows the Run key, which starts the service at the next login only: a copy that runs already is stopped
// (an update brings a new version) and the service starts now in the background, so "now and at every login" holds
// without a scheduled task.
func (s *service) startedNow() {
	s.replaceRunning() // an update: the running copy (an older version, perhaps) makes room; one that won't, waits
	if s.startNow == nil {
		return
	}
	if err := s.startNow(s.exe, s.commandArgs()); err != nil {
		s.printf("could not start it now (%v): it starts at the next login, or run \"%s\"\n", err, s.k().what)
		return
	}
	s.printf("started it now in the background\n")
}

// addRunKey puts the command into the user's Run key (Windows; it starts at the next login).
func (s *service) addRunKey() error {
	if out, err := s.run("reg", "add", serviceRunKey, "/v", s.k().task, "/t", "REG_SZ", "/d", s.command(), "/f"); err != nil {
		return fmt.Errorf("reg add %s: %v: %s", serviceRunKey, err, out)
	}
	s.runKey = true
	return nil
}

// install writes the service's file, stops a copy that runs already and starts the service.
func (s *service) install() error {
	cmds := s.installCmds()
	if cmds == nil {
		return fmt.Errorf("--install: not on %s (macOS, Linux and Windows only)", s.goos)
	}
	if s.goos == "windows" && len(s.command()) > schtasksMaxTR {
		if err := s.addRunKey(); err != nil {
			return err
		}
		s.printf("the command is longer than a scheduled task takes (%d characters, at most %d): added to %s instead (starts at every login)\n",
			len(s.command()), schtasksMaxTR, serviceRunKey)
		s.startedNow()
		return nil
	}
	if p := s.file(); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(s.content()), 0o600); err != nil {
			return err
		}
		_ = os.Chmod(p, 0o600)
	}
	if l := s.logFile(); l != "" {
		_ = os.MkdirAll(filepath.Dir(l), 0o755)
	}
	replaceAt := 0 // the commands from here on start the service
	if s.goos == "windows" {
		replaceAt = 1 // schtasks /Run, once the task is there
	}
	for i, c := range cmds {
		if i == replaceAt {
			s.replaceRunning()
		}
		out, err := s.run(c[0], c[1:]...)
		if err != nil && s.goos == "darwin" && i == 1 {
			out, err = s.retryBootstrap(c, out, err)
		}
		if err == nil || s.goos == "darwin" && i == 0 { // bootout: an older one may not be there
			continue
		}
		if s.goos == "windows" && i == 0 {
			// a scheduled task at logon may need rights this user does not have: the user's Run key does it too
			if s.addRunKey() == nil {
				s.printf("the scheduled task was refused (%s): added to %s instead (starts at every login; no admin rights needed)\n", out, serviceRunKey)
				s.startedNow()
				return nil
			}
		}
		return fmt.Errorf("%s: %v: %s", strings.Join(c, " "), err, out)
	}
	return nil
}

// retryBootstrap tries `launchctl bootstrap` again after it failed, for up to bootWait. `launchctl bootout` returns
// before the earlier service is gone: launchd gives its process time to end (a worker that unloads a model takes a
// while), and until then bootstrap of the same label fails ("Bootstrap failed: 5: Input/output error") — an update
// of a running service ended there, with no service at all once the old process was gone.
func (s *service) retryBootstrap(c []string, out string, err error) (string, error) {
	said := false
	for end := time.Now().Add(s.bootWait); err != nil && time.Now().Before(end); {
		if !said {
			s.printf("the earlier service is still ending: waiting for it (up to %s)\n", s.bootWait)
			said = true
		}
		time.Sleep(s.bootPoll)
		out, err = s.run(c[0], c[1:]...)
	}
	return out, err
}

// uninstall stops it and removes its file.
func (s service) uninstall() error {
	for _, c := range s.uninstallCmds() {
		_, _ = s.run(c[0], c[1:]...)
	}
	if p := s.file(); p != "" {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if s.goos == "linux" {
			_, _ = s.run("systemctl", "--user", "daemon-reload")
		}
	}
	return nil
}

// describe says what was installed, for the person who ran it.
func (s service) describe() string {
	k := s.k()
	switch s.goos {
	case "darwin":
		return fmt.Sprintf("installed %s: %s starts at login (now too)\n  log: %s", s.file(), k.what, s.logFile())
	case "linux":
		return fmt.Sprintf("installed %s: %s starts at login (now too)\n  log: journalctl --user -u %s", s.file(), k.what, k.unit)
	}
	if s.runKey {
		return "installed \"" + k.task + "\" in " + serviceRunKey + ": " + k.what + " starts at login"
	}
	return "installed the scheduled task \"" + k.task + "\": " + k.what + " starts at login (now too)"
}
