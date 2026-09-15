package config

import (
	"strings"
	"testing"
	"time"
)

// triggerYAML wraps one trigger rule body in a config that otherwise has
// nothing to complain about, so each test reads as the rule it is about.
func triggerYAML(rule string) string {
	return "mounts:\n  - path: /tmp/m\ntriggers:\n  - name: t\n    paths: [\"/work/**\"]\n" + rule
}

func mustFail(t *testing.T, yaml, want string) {
	t.Helper()
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatalf("parsed without error, wanted %q\n%s", want, yaml)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not mention %q", err, want)
	}
}

func TestTriggerNeedsExactlyOneAction(t *testing.T) {
	mustFail(t, triggerYAML("    action: {}\n"), "exactly one action")
	mustFail(t, triggerYAML(""), "exactly one action")
	mustFail(t, triggerYAML(`    action:
      exec: { command: ["/bin/true"] }
      webhook: { url: https://hooks.example/x, secret: "keyring:cloudfs/hook" }
`), "exactly one action")
	c, err := Parse([]byte(triggerYAML("    action:\n      exec: { command: [\"/bin/true\"] }\n")))
	if err != nil {
		t.Fatal(err)
	}
	if c.Triggers[0].Action.Exec == nil || c.Triggers[0].Action.Webhook != nil {
		t.Fatalf("action = %+v", c.Triggers[0].Action)
	}
}

func TestWebhookSecretMustBeAReference(t *testing.T) {
	mustFail(t, triggerYAML("    action:\n      webhook: { url: https://hooks.example/x, secret: hunter2 }\n"), "keyring:")
	mustFail(t, triggerYAML("    action:\n      webhook: { url: https://hooks.example/x }\n"), "secret")
	for _, ref := range []string{"keyring:cloudfs/hook", "secretfile:hook"} {
		if _, err := Parse([]byte(triggerYAML("    action:\n      webhook: { url: https://hooks.example/x, secret: \"" + ref + "\" }\n"))); err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
	}
	if !IsSecretField("secret") {
		t.Fatal("secret is not treated as a secret field")
	}
}

func TestPlainHTTPWebhookNeedsInsecure(t *testing.T) {
	hook := func(url string, insecure bool) string {
		extra := ""
		if insecure {
			extra = ", insecure: true"
		}
		return triggerYAML("    action:\n      webhook: { url: \"" + url + "\", secret: \"keyring:cloudfs/hook\"" + extra + " }\n")
	}
	mustFail(t, hook("http://hooks.example/x", false), "insecure: true")
	mustFail(t, hook("ftp://hooks.example/x", true), "https")
	mustFail(t, hook("hooks.example/x", true), "https")
	for _, ok := range []string{"https://hooks.example/x", "http://127.0.0.1:8080/x", "http://localhost/x", "http://[::1]:9/x"} {
		if _, err := Parse([]byte(hook(ok, false))); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	if _, err := Parse([]byte(hook("http://hooks.example/x", true))); err != nil {
		t.Fatalf("insecure: true did not allow plain http: %v", err)
	}
}

func TestExecRuleWithoutOriginFilterWarns(t *testing.T) {
	exec := "    action:\n      exec: { command: [\"/bin/true\", \"{path}\"] }\n"
	c, err := Parse([]byte(triggerYAML(exec)))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "trigger t") || !strings.Contains(c.Warnings[0], "origins: [kernel, remote]") {
		t.Fatalf("warnings = %q", c.Warnings)
	}
	c, err = Parse([]byte(triggerYAML("    origins: [kernel, api]\n" + exec)))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 1 {
		t.Fatalf("an explicit api origin should still warn, got %q", c.Warnings)
	}
	c, err = Parse([]byte(triggerYAML("    origins: [kernel, remote]\n" + exec)))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("excluding api still warned: %q", c.Warnings)
	}
	// A webhook cannot write back through the API, so it never warns.
	c, err = Parse([]byte(triggerYAML("    action:\n      webhook: { url: https://hooks.example/x, secret: \"keyring:cloudfs/hook\" }\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("a webhook rule warned: %q", c.Warnings)
	}
	// Validate owns the slice: a second pass does not double the warnings.
	c, _ = Parse([]byte(triggerYAML(exec)))
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 1 {
		t.Fatalf("warnings accumulated across Validate calls: %q", c.Warnings)
	}
}

func TestPlaceholdersMustBeWholeArgvElements(t *testing.T) {
	exec := func(argv string) string {
		return triggerYAML("    origins: [kernel]\n    action:\n      exec: { command: [" + argv + "] }\n")
	}
	mustFail(t, exec(`"/usr/bin/claude", "-p", "Summarize {path}"`), "whole argv element")
	mustFail(t, exec(`"/usr/bin/claude", "{path}.bak"`), "whole argv element")
	mustFail(t, exec(`"{path}"`), "command[0]")
	mustFail(t, exec(`"/bin/{path}"`), "command[0]")
	mustFail(t, exec(``), "command")
	mustFail(t, exec(`""`), "command")
	// {prompt} is what agents receive; a trigger has none to substitute.
	mustFail(t, exec(`"/usr/bin/claude", "-p", "{prompt}"`), "{prompt}")
	c, err := Parse([]byte(exec(`"/usr/bin/summarize", "{path}", "{kind}", "{uri}", "--braces", "{not-a-placeholder}"`)))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Triggers[0].Action.Exec.Command; len(got) != 6 || got[1] != "{path}" {
		t.Fatalf("argv = %q", got)
	}
}

func TestTriggerDefaultsAreFilled(t *testing.T) {
	c, err := Parse([]byte(triggerYAML("    origins: [kernel]\n    action:\n      exec: { command: [\"/bin/true\"], cwd: \"~/work\" }\n") +
		"  - name: hook\n    paths: [\"/work/reports/**\"]\n    action:\n      webhook: { url: https://hooks.example/x, secret: \"keyring:cloudfs/hook\" }\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Triggers[0]
	if strings.Join(r.Events, ",") != "write,create,mkdir,remove,rename,remote,rescan" {
		t.Fatalf("events = %q", r.Events)
	}
	if r.Debounce != 2*time.Second || r.OnRescan != "deliver" {
		t.Fatalf("debounce = %s, on_rescan = %q", r.Debounce, r.OnRescan)
	}
	if r.Action.Exec.Timeout != 10*time.Minute {
		t.Fatalf("exec timeout = %s", r.Action.Exec.Timeout)
	}
	if strings.HasPrefix(r.Action.Exec.Cwd, "~") {
		t.Fatalf("cwd not expanded: %s", r.Action.Exec.Cwd)
	}
	h := c.Triggers[1]
	if strings.Join(h.Origins, ",") != "kernel,api,remote" {
		t.Fatalf("origins = %q", h.Origins)
	}
	if h.Action.Webhook.Timeout != 15*time.Second || h.Action.Webhook.IncludeDownloadURL || h.Action.Webhook.Insecure {
		t.Fatalf("webhook = %+v", h.Action.Webhook)
	}
	// Explicit values win over defaults, and the known lists are enforced.
	c, err = Parse([]byte(triggerYAML("    events: [write, remove]\n    origins: [remote]\n    debounce: 500ms\n    on_rescan: ignore\n    action:\n      exec: { command: [\"/bin/true\"], timeout: 1m }\n")))
	if err != nil {
		t.Fatal(err)
	}
	r = c.Triggers[0]
	if strings.Join(r.Events, ",") != "write,remove" || strings.Join(r.Origins, ",") != "remote" || r.Debounce != 500*time.Millisecond || r.OnRescan != "ignore" || r.Action.Exec.Timeout != time.Minute {
		t.Fatalf("explicit values lost: %+v", r)
	}
	mustFail(t, triggerYAML("    events: [touch]\n    action:\n      exec: { command: [\"/bin/true\"] }\n"), "events")
	mustFail(t, triggerYAML("    origins: [fuse]\n    action:\n      exec: { command: [\"/bin/true\"] }\n"), "origins")
	mustFail(t, triggerYAML("    on_rescan: skip\n    action:\n      exec: { command: [\"/bin/true\"] }\n"), "on_rescan")
	mustFail(t, triggerYAML("    debounce: -1s\n    action:\n      exec: { command: [\"/bin/true\"] }\n"), "debounce")
	mustFail(t, "mounts:\n  - path: /tmp/m\ntriggers:\n  - name: t\n    action:\n      exec: { command: [\"/bin/true\"] }\n", "paths")
	mustFail(t, "mounts:\n  - path: /tmp/m\ntriggers:\n  - name: t\n    paths: [\"work/**\"]\n    action:\n      exec: { command: [\"/bin/true\"] }\n", "absolute")
	mustFail(t, "mounts:\n  - path: /tmp/m\ntriggers:\n  - name: t\n    paths: [\"/work/[\"]\n    action:\n      exec: { command: [\"/bin/true\"] }\n", "pattern")
	mustFail(t, triggerYAML("    action:\n      webhook: { url: https://hooks.example/x, secret: \"keyring:cloudfs/hook\", proxy: nowhere }\n"), "proxy")
}

func TestAgentsShareTheExecRules(t *testing.T) {
	agent := func(body string) string {
		return "mounts:\n  - path: /tmp/m\nagents:\n  - name: claude\n" + body
	}
	mustFail(t, agent("    exec: { command: [] }\n"), "command")
	mustFail(t, agent("    exec: { command: [\"{prompt}\"] }\n"), "command[0]")
	mustFail(t, agent("    exec: { command: [\"claude\", \"-p\", \"Do this: {prompt}\"] }\n"), "whole argv element")
	mustFail(t, agent("    exec: { command: [\"claude\"], timeout: -1s }\n"), "timeout")
	c, err := Parse([]byte(agent("    exec: { command: [\"claude\", \"-p\", \"{prompt}\", \"{path}\"], cwd: \"~\" }\n")))
	if err != nil {
		t.Fatal(err)
	}
	a := c.Agents[0]
	if a.Exec.Timeout != 10*time.Minute || strings.HasPrefix(a.Exec.Cwd, "~") || a.Exec.Cwd == "" {
		t.Fatalf("agent exec = %+v", a.Exec)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("an agent is invoked on purpose and must not warn: %q", c.Warnings)
	}
}

func TestTriggerNamesAreValidatedAndUnique(t *testing.T) {
	rule := func(name string) string {
		return "  - name: " + name + "\n    paths: [\"/work/**\"]\n    origins: [kernel]\n    action:\n      exec: { command: [\"/bin/true\"] }\n"
	}
	head := "mounts:\n  - path: /tmp/m\ntriggers:\n"
	for _, bad := range []string{"\"\"", "Inbox", "-x", "a_b", "\"a:b\"", strings.Repeat("a", 65)} {
		mustFail(t, head+rule(bad), "name")
	}
	mustFail(t, head+rule("same")+rule("same"), "same")
	if _, err := Parse([]byte(head + rule("a") + rule("inbox-to-agent") + rule(strings.Repeat("b", 64)))); err != nil {
		t.Fatal(err)
	}
	agents := "mounts:\n  - path: /tmp/m\nagents:\n  - name: dup\n    exec: { command: [\"x\"] }\n  - name: dup\n    exec: { command: [\"y\"] }\n"
	mustFail(t, agents, "dup")
	mustFail(t, "mounts:\n  - path: /tmp/m\nagents:\n  - name: Claude\n    exec: { command: [\"x\"] }\n", "name")
	// A trigger and an agent may share a name: deliveries prefix agents
	// with "agent:", which the name rule keeps out of trigger names.
	if _, err := Parse([]byte(head + rule("claude") + "agents:\n  - name: claude\n    exec: { command: [\"x\"] }\n")); err != nil {
		t.Fatal(err)
	}
}

// roadmapTriggers is the example of docs/agent-roadmap.md §5.2, kept in
// step with it by hand: what the design shows must parse as written.
const roadmapTriggers = `
mounts:
  - path: /tmp/m
proxy:
  outbounds:
    - { name: clash, type: socks5, addr: 127.0.0.1:7890 }
triggers:
  - name: inbox-to-agent
    paths: ["/work/inbox/**"]        # glob（自带匹配器，无新依赖），挂载相对路径
    events: [create, write, rename]  # 默认全部；remote = delta/刷新发现
    origins: [kernel, remote]        # 默认全部；排除 api 可避免 agent 自触发
    debounce: 2s
    on_rescan: ignore                # 默认 deliver
    action:
      exec: { command: ["/usr/local/bin/summarize", "{path}"], cwd: ~/work, timeout: 10m }  # 占位符只能是独立的 argv 元素
  - name: notify
    paths: ["/work/reports/**"]
    events: [write]
    action:
      webhook: { url: https://hooks.example/cloudfs, secret: keyring:cloudfs/hook, timeout: 15s, include_download_url: false, proxy: direct }
agents:
  - name: claude
    exec: { command: ["claude", "-p", "{prompt}"], cwd: "~", timeout: 30m }
`

func TestRoadmapTriggerExampleParses(t *testing.T) {
	c, err := Parse([]byte(roadmapTriggers))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Triggers) != 2 || len(c.Agents) != 1 {
		t.Fatalf("triggers = %d, agents = %d", len(c.Triggers), len(c.Agents))
	}
	inbox, notify := c.Triggers[0], c.Triggers[1]
	if inbox.Name != "inbox-to-agent" || inbox.OnRescan != "ignore" || inbox.Action.Exec == nil || inbox.Action.Exec.Timeout != 10*time.Minute {
		t.Fatalf("inbox = %+v", inbox)
	}
	if strings.HasPrefix(inbox.Action.Exec.Cwd, "~") {
		t.Fatalf("cwd not expanded: %s", inbox.Action.Exec.Cwd)
	}
	if notify.Action.Webhook == nil || notify.Action.Webhook.Secret != "keyring:cloudfs/hook" || notify.Action.Webhook.Proxy != "direct" || notify.Action.Webhook.Timeout != 15*time.Second {
		t.Fatalf("notify = %+v", notify.Action.Webhook)
	}
	if strings.Join(notify.Origins, ",") != "kernel,api,remote" || notify.Debounce != 2*time.Second || notify.OnRescan != "deliver" {
		t.Fatalf("notify defaults = %+v", notify)
	}
	if c.Agents[0].Exec.Timeout != 30*time.Minute || strings.HasPrefix(c.Agents[0].Exec.Cwd, "~") {
		t.Fatalf("agent = %+v", c.Agents[0].Exec)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("the documented example must be warning-free, got %q", c.Warnings)
	}
}
