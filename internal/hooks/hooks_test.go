package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestInstallIsIdempotentAndPreservesOtherHooks: install merges our three
// groups into an existing settings file, keeps the person's own hooks,
// changes nothing on a second run, converges an older shape of our own
// group, and uninstall removes only ours.
func TestInstallIsIdempotentAndPreservesOtherHooks(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Beside the person's own groups sits a group of ours under Stop, the
	// session-end event of an earlier shape: install must move it.
	existing := `{"model": "opus", "hooks": {"PostToolUse": [{"matcher": "Write", "hooks": [{"type": "command", "command": "my-formatter"}]}], "SessionStart": [{"hooks": [{"type": "command", "command": "echo hi && cloudfs agent-hook prompt"}]}], "Stop": [{"hooks": [{"type": "command", "command": "sh -c 'cloudfs agent-hook stop --client claude'"}]}, {"hooks": [{"type": "command", "command": "my-notifier"}]}]}}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Install(home, []string{"claude"})
	if err != nil || len(res) != 1 || !res[0].Changed || res[0].Path != path {
		t.Fatalf("%+v %v", res, err)
	}
	first, _ := os.ReadFile(path)
	var root map[string]any
	if err := json.Unmarshal(first, &root); err != nil {
		t.Fatal(err)
	}
	if root["model"] != "opus" {
		t.Fatal("unrelated settings lost")
	}
	hooks := root["hooks"].(map[string]any)
	if len(hooks["PostToolUse"].([]any)) != 3 || len(hooks["UserPromptSubmit"].([]any)) != 1 || len(hooks["SessionEnd"].([]any)) != 1 {
		t.Fatalf("groups: %v", hooks)
	}
	// The per-turn Stop is not where the session ends: our stale group is
	// gone from it, the person's own stays.
	if stop := hooks["Stop"].([]any); len(stop) != 1 || !strings.Contains(fmt.Sprint(stop[0]), "my-notifier") {
		t.Fatalf("Stop after install: %v", stop)
	}
	if start := hooks["SessionStart"].([]any); len(start) != 1 {
		t.Fatalf("an event we never registered under was touched: %v", start)
	}
	post := hooks["PostToolUse"].([]any)
	if post[0].(map[string]any)["matcher"] != "Write" {
		t.Fatal("the person's own group moved")
	}
	ours := post[1].(map[string]any)
	if ours["matcher"] != "Read|Grep|Bash" {
		t.Fatalf("read matcher: %v", ours)
	}
	cmd := ours["hooks"].([]any)[0].(map[string]any)["command"].(string)
	if !strings.HasPrefix(cmd, "sh -c '") || !strings.Contains(cmd, "cloudfs agent-hook read --client claude") || !strings.Contains(cmd, "/cloudfs/mounts") {
		t.Fatalf("command: %s", cmd)
	}
	// The write group reports the client's own writes, so the next turn
	// does not hand them back as someone else's.
	wrote := post[2].(map[string]any)
	if wrote["matcher"] != "Write|Edit|MultiEdit|NotebookEdit" || !strings.Contains(fmt.Sprint(wrote["hooks"]), "cloudfs agent-hook write --client claude") {
		t.Fatalf("write matcher: %v", wrote)
	}
	if res, err := Install(home, []string{"claude"}); err != nil || res[0].Changed {
		t.Fatalf("second install: %+v %v", res, err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatalf("second install rewrote the file:\n%s\n---\n%s", first, second)
	}
	// An older shape of our group is converged, not duplicated.
	old := strings.Replace(string(second), `"Read|Grep|Bash"`, `"Read"`, 1)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, _ := Install(home, []string{"claude"}); !res[0].Changed {
		t.Fatal("an outdated group was not converged")
	}
	third, _ := os.ReadFile(path)
	if string(third) != string(second) {
		t.Fatalf("converged file differs:\n%s", third)
	}
	st := Statuses(home)
	if st[0].Client != "claude" || !st[0].Installed || !st[0].Present || st[1].Installed {
		t.Fatalf("status: %+v", st)
	}
	res, err = Uninstall(home, []string{"claude"})
	if err != nil || !res[0].Changed {
		t.Fatalf("uninstall: %+v %v", res, err)
	}
	after, _ := os.ReadFile(path)
	root = map[string]any{}
	_ = json.Unmarshal(after, &root)
	hooks = root["hooks"].(map[string]any)
	if _, ok := hooks["UserPromptSubmit"]; ok {
		t.Fatal("our prompt group survived uninstall")
	}
	if _, ok := hooks["SessionEnd"]; ok {
		t.Fatal("our session-end group survived uninstall")
	}
	if stop := hooks["Stop"].([]any); len(stop) != 1 || !strings.Contains(fmt.Sprint(stop[0]), "my-notifier") {
		t.Fatalf("uninstall touched the person's Stop hook: %v", stop)
	}
	if len(hooks["PostToolUse"].([]any)) != 1 || len(hooks["SessionStart"].([]any)) != 1 {
		t.Fatalf("uninstall touched other hooks: %v", hooks)
	}
	if res, _ := Uninstall(home, []string{"claude"}); res[0].Changed {
		t.Fatal("second uninstall changed something")
	}
	if _, err := Install(home, []string{"emacs"}); err == nil || !strings.Contains(err.Error(), "unknown client") {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallCreatesTheFileAndDetectsClients(t *testing.T) {
	home := t.TempDir()
	if got := Detect(home); len(got) != 0 {
		t.Fatalf("detected %v in an empty home", got)
	}
	if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := Install(home, nil)
	if err != nil || len(res) != 1 || res[0].Client != "gemini" || !res[0].Changed {
		t.Fatalf("%+v %v", res, err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, data)
	}
	hooks := root["hooks"].(map[string]any)
	for _, ev := range []string{"BeforeAgent", "AfterTool", "SessionEnd"} {
		if _, ok := hooks[ev]; !ok {
			t.Fatalf("gemini lacks %s: %v", ev, hooks)
		}
	}
	if res, err := Install(home, []string{"codex"}); err != nil || res[0].Note == "" {
		t.Fatalf("codex must carry its enable-hooks note: %+v %v", res, err)
	}
}

// TestGuardExitsBeforeSpawningOutsideAMount runs the real guard under
// sh: outside a registered mount it exits 0 without reaching the exec;
// inside one (or below one) it reaches it; without a registry it exits.
func TestGuardExitsBeforeSpawningOutsideAMount(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	mount := filepath.Join(home, "cloud")
	inside := filepath.Join(mount, "project", "deep")
	outside := filepath.Join(home, "elsewhere")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(dir string, env ...string) string {
		// The exec target is replaced by a marker so the test sees whether
		// the guard let the command through.
		script := Guard() + `echo REACHED`
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = dir
		cmd.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sh in %s: %v: %s", dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(inside); got != "" {
		t.Fatalf("no registry, yet the guard let the command through: %q", got)
	}
	if err := WriteMounts(MountsPath(home), []string{mount, "/other/mount"}); err != nil {
		t.Fatal(err)
	}
	// cloudfs must be on PATH for the guard to pass; a stub does.
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "cloudfs"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	withBin := "PATH=" + bin + ":" + os.Getenv("PATH")
	if got := run(inside, withBin); got != "REACHED" {
		t.Fatalf("inside a mount: %q", got)
	}
	if got := run(mount, withBin); got != "REACHED" {
		t.Fatalf("at the mount root: %q", got)
	}
	if got := run(outside, withBin); got != "" {
		t.Fatalf("outside every mount: %q", got)
	}
	// A registry entry that is a prefix but not a parent must not match.
	if err := os.MkdirAll(filepath.Join(home, "cloudy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := run(filepath.Join(home, "cloudy"), withBin); got != "" {
		t.Fatalf("prefix-only match: %q", got)
	}
	// CLAUDE_PROJECT_DIR wins over the working directory.
	if got := run(outside, withBin, "CLAUDE_PROJECT_DIR="+inside); got != "REACHED" {
		t.Fatalf("project dir: %q", got)
	}
	mounts, err := ReadMounts(MountsPath(home))
	if err != nil || len(mounts) != 2 || mounts[0] != "/other/mount" {
		t.Fatalf("registry: %v %v", mounts, err)
	}
}

func TestReadPathsMinesToolInputs(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a.md", "b.go"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ev := Event{CWD: dir, ToolName: "Read", ToolInput: json.RawMessage(`{"file_path": "a.md"}`)}
	if got := ReadPaths(ev); len(got) != 1 || got[0] != filepath.Join(dir, "a.md") {
		t.Fatalf("read: %v", got)
	}
	ev = Event{CWD: dir, ToolName: "Bash", ToolInput: json.RawMessage(`{"command": "cat a.md | grep x; wc -l ./b.go missing.txt -n"}`)}
	got := ReadPaths(ev)
	if len(got) != 2 || got[0] != filepath.Join(dir, "a.md") || got[1] != filepath.Join(dir, "b.go") {
		t.Fatalf("bash: %v", got)
	}
	ev = Event{CWD: dir, ToolName: "Glob", ToolInput: json.RawMessage(`{"path": "` + dir + `"}`)}
	if got := ReadPaths(ev); len(got) != 0 {
		t.Fatalf("a directory is not a read: %v", got)
	}
	if got := ReadPaths(Event{}); got != nil {
		t.Fatalf("empty: %v", got)
	}
}

func TestParseEventTakesSessionAndCwd(t *testing.T) {
	ev := ParseEvent(strings.NewReader(`{"session_id": "s-1", "cwd": "/tmp/x", "tool_name": "Read", "tool_input": {"file_path": "/a"}}`))
	if ev.SessionID != "s-1" || ev.CWD != "/tmp/x" || ev.ToolName != "Read" {
		t.Fatalf("%+v", ev)
	}
	if ev := ParseEvent(strings.NewReader("not json")); ev.SessionID != "" || ev.CWD == "" {
		t.Fatalf("malformed: %+v", ev)
	}
}

// TestHookGuardIsPureShell: the guard every hook command starts with
// names no executable but the shell's own builtins, grep and cloudfs —
// no python, no jq, no curl — so a turn outside a mount costs one grep
// of a small file and spawns nothing else; and the command templates add
// only cloudfs itself. The runtime half (a real sh, strace-free) is
// TestGuardExitsBeforeSpawningOutsideAMount.
func TestHookGuardIsPureShell(t *testing.T) {
	guard := Guard()
	allowed := map[string]bool{"cd": true, "exit": true, "grep": true, "break": true, "case": true, "esac": true, "in": true, "while": true, "do": true, "done": true, "command": true, "-v": true, "cloudfs": true, "d=$PWD;": true}
	// Every word that could be a program: the first word of each ';' or
	// '||' or '&&' separated command, and the word after 'command -v'.
	for _, stmt := range regexp.MustCompile(`\|\||&&|;`).Split(guard, -1) {
		fields := strings.Fields(stmt)
		if len(fields) == 0 {
			continue
		}
		head := fields[0]
		if strings.Contains(head, "=") || strings.HasPrefix(head, "[") || strings.HasPrefix(head, "$") || strings.HasPrefix(head, "\"") {
			continue
		}
		if !allowed[head] {
			t.Errorf("the guard runs %q: %s", head, stmt)
		}
	}
	for _, bad := range []string{"python", "jq", "curl", "node", "perl", "awk", "sed", "find", "stat", "cat", "$(", "`"} {
		if strings.Contains(guard, bad) {
			t.Errorf("the guard uses %q", bad)
		}
	}
	for _, event := range []string{"prompt", "read", "stop"} {
		cmd := Command(event, "claude")
		if !strings.HasPrefix(cmd, "sh -c '"+guard) {
			t.Errorf("%s command does not start with the guard: %s", event, cmd)
		}
		rest := strings.TrimPrefix(cmd, "sh -c '"+guard)
		for _, word := range strings.Fields(rest) {
			if word == "exec" || word == "cloudfs" || word == "agent-hook" || word == event || strings.HasPrefix(word, "--") || word == "claude" || strings.HasPrefix(word, ">") || strings.HasPrefix(word, "2>") || word == "||" || word == "true'" {
				continue
			}
			t.Errorf("%s command runs %q: %s", event, word, rest)
		}
	}
}
