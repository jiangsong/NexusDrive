package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Trigger is one event rule (docs/agent-roadmap.md §5.2): a change that
// matches Paths, Events and Origins fires Action, after Debounce has
// collapsed a burst into one delivery. The owner daemon evaluates rules;
// nothing else in the process accepts a command from anywhere but this
// file.
type Trigger struct {
	// Name identifies the rule in deliveries, the console and the CLI.
	Name string `yaml:"name"`
	// Paths are absolute virtual globs in the pathglob dialect ("**"
	// crosses directories, "*" does not).
	Paths []string `yaml:"paths"`
	// Events holds change kinds from TriggerEvents; nil means all of them.
	Events []string `yaml:"events"`
	// Origins holds change sources from TriggerOrigins; nil means all of
	// them. Excluding "api" keeps an exec rule from firing on the agent's
	// own writes.
	Origins []string `yaml:"origins"`
	// Debounce is how long a delivery waits for more changes to the same
	// path before it runs.
	Debounce time.Duration `yaml:"debounce"`
	// OnRescan says what a whole-tree rescan (queue overflow, unresolved
	// path) does: OnRescanDeliver queues one delivery with an empty path,
	// OnRescanIgnore drops it.
	OnRescan string        `yaml:"on_rescan"`
	Action   TriggerAction `yaml:"action"`
}

// TriggerAction holds exactly one of the two action kinds.
type TriggerAction struct {
	Exec    *ExecAction    `yaml:"exec"`
	Webhook *WebhookAction `yaml:"webhook"`
}

// ExecAction runs Command without a shell. A placeholder from
// ExecPlaceholders is substituted only when it is a whole argv element;
// "Summarize {path}" is rejected because a path is never spliced into
// another string.
type ExecAction struct {
	Command []string `yaml:"command"`
	// Cwd is the working directory; "~" expands to the home directory.
	Cwd string `yaml:"cwd"`
	// Timeout kills the process group when it expires.
	Timeout time.Duration `yaml:"timeout"`
}

// WebhookAction posts a signed JSON body to URL (docs/agent-roadmap.md
// §5.5).
type WebhookAction struct {
	// URL is https, or http to a loopback address; anything else needs
	// Insecure.
	URL string `yaml:"url"`
	// Secret signs the body. It must be a keyring: or secretfile:
	// reference, never the key itself.
	Secret  string        `yaml:"secret"`
	Timeout time.Duration `yaml:"timeout"`
	// IncludeDownloadURL adds a signed direct link to the body, which hands
	// that link to whoever receives the hook.
	IncludeDownloadURL bool `yaml:"include_download_url"`
	// Proxy names an outbound or group; empty follows the proxy rules.
	Proxy string `yaml:"proxy"`
	// Insecure allows a plain-http URL that is not loopback.
	Insecure bool `yaml:"insecure"`
}

// Agent is a command the console can run on a file on request
// (docs/agent-roadmap.md §5.7). It shares ExecAction's rules and may take
// "{prompt}", which a trigger rule has nothing to fill with.
type Agent struct {
	Name string     `yaml:"name"`
	Exec ExecAction `yaml:"exec"`
}

// TriggerEvents lists every change kind a rule may name. The names are
// vfs.ChangeKind's String values, repeated here because config sits below
// vfs; vfs.TestTriggerEventNamesMatchVFS keeps the two in step.
var TriggerEvents = []string{"write", "create", "mkdir", "remove", "rename", "remote", "rescan"}

// TriggerOrigins lists every change source a rule may name, as
// vfs.Origin's String values.
var TriggerOrigins = []string{"kernel", "api", "remote"}

// ExecPlaceholders are the argv elements the exec runner substitutes.
var ExecPlaceholders = []string{"{path}", "{kind}", "{uri}", "{prompt}"}

// OnRescan values.
const (
	OnRescanDeliver = "deliver"
	OnRescanIgnore  = "ignore"
)

// Built-in trigger defaults, applied where the YAML is silent.
const (
	DefaultTriggerDebounce = 2 * time.Second
	DefaultExecTimeout     = 10 * time.Minute
	DefaultWebhookTimeout  = 15 * time.Second
)

// ruleName is the shape of a trigger or agent name: a DNS-label-like token
// that is safe in a URL, a delivery row and a log line. It has no ":", so
// the "agent:<name>" delivery rule namespace can never collide with a
// trigger name.
var ruleName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// validateTriggers checks the triggers and agents sections, fills their
// defaults and appends the self-trigger warning to c.Warnings. proxies
// holds every outbound and group name, plus "direct".
func (c *Config) validateTriggers(proxies map[string]bool) error {
	seen := make(map[string]bool, len(c.Triggers))
	for i := range c.Triggers {
		r := &c.Triggers[i]
		key := fmt.Sprintf("triggers[%d]", i)
		if !ruleName.MatchString(r.Name) {
			return fmt.Errorf("config: %s.name %q must match %s", key, r.Name, ruleName)
		}
		if seen[r.Name] {
			return fmt.Errorf("config: %s repeats the trigger name %q", key, r.Name)
		}
		seen[r.Name] = true
		key = "trigger " + r.Name
		if len(r.Paths) == 0 {
			return fmt.Errorf("config: %s: paths must name at least one glob", key)
		}
		for _, p := range r.Paths {
			if !strings.HasPrefix(p, "/") {
				return fmt.Errorf("config: %s: paths entry %q must be an absolute virtual glob such as \"/work/**\"", key, p)
			}
		}
		if err := checkIndexGlobs(key+".paths", r.Paths); err != nil {
			return err
		}
		if r.Events == nil {
			r.Events = append([]string(nil), TriggerEvents...)
		}
		if err := knownNames(key+".events", r.Events, TriggerEvents); err != nil {
			return err
		}
		if r.Origins == nil {
			r.Origins = append([]string(nil), TriggerOrigins...)
		}
		if err := knownNames(key+".origins", r.Origins, TriggerOrigins); err != nil {
			return err
		}
		if r.Debounce == 0 {
			r.Debounce = DefaultTriggerDebounce
		}
		if r.Debounce < 0 {
			return fmt.Errorf("config: %s: debounce must not be negative, got %s", key, r.Debounce)
		}
		switch r.OnRescan {
		case "":
			r.OnRescan = OnRescanDeliver
		case OnRescanDeliver, OnRescanIgnore:
		default:
			return fmt.Errorf("config: %s: on_rescan must be %s or %s, got %q", key, OnRescanDeliver, OnRescanIgnore, r.OnRescan)
		}
		if (r.Action.Exec == nil) == (r.Action.Webhook == nil) {
			return fmt.Errorf("config: %s: action must hold exactly one action, exec or webhook", key)
		}
		if r.Action.Exec != nil {
			if err := r.Action.Exec.validate(key+".action.exec", false); err != nil {
				return err
			}
			if hasName(r.Origins, "api") {
				c.Warnings = append(c.Warnings, fmt.Sprintf("trigger %s: exec may be fired by the agent's own writes; set origins: [kernel, remote]", r.Name))
			}
		}
		if r.Action.Webhook != nil {
			if err := r.Action.Webhook.validate(key+".action.webhook", proxies); err != nil {
				return err
			}
		}
	}
	seen = make(map[string]bool, len(c.Agents))
	for i := range c.Agents {
		a := &c.Agents[i]
		if !ruleName.MatchString(a.Name) {
			return fmt.Errorf("config: agents[%d].name %q must match %s", i, a.Name, ruleName)
		}
		if seen[a.Name] {
			return fmt.Errorf("config: agents[%d] repeats the agent name %q", i, a.Name)
		}
		seen[a.Name] = true
		if err := a.Exec.validate("agent "+a.Name+".exec", true); err != nil {
			return err
		}
	}
	return nil
}

// validate fills the exec defaults and enforces the argv rules: a command,
// a literal argv[0], placeholders only as whole elements, and {prompt}
// only where something supplies a prompt.
func (a *ExecAction) validate(key string, prompt bool) error {
	if len(a.Command) == 0 || a.Command[0] == "" {
		return fmt.Errorf("config: %s: command must start with the program to run", key)
	}
	if strings.Contains(a.Command[0], "{") {
		return fmt.Errorf("config: %s: command[0] %q is run as written and cannot hold a placeholder", key, a.Command[0])
	}
	for i, arg := range a.Command[1:] {
		for _, ph := range ExecPlaceholders {
			if arg == ph {
				if ph == "{prompt}" && !prompt {
					return fmt.Errorf("config: %s: command[%d] uses {prompt}, which only agents receive; a trigger can pass {path}, {kind} or {uri}", key, i+1)
				}
				break
			}
			if strings.Contains(arg, ph) {
				return fmt.Errorf("config: %s: command[%d] %q embeds %s in a longer string; a placeholder is substituted only as a whole argv element, so write it as its own element (for example \"Summarize\", \"%s\")", key, i+1, arg, ph, ph)
			}
		}
	}
	a.Cwd = ExpandHome(a.Cwd)
	if a.Cwd != "" && !filepath.IsAbs(a.Cwd) {
		return fmt.Errorf("config: %s: cwd %q must be an absolute directory", key, a.Cwd)
	}
	if a.Timeout == 0 {
		a.Timeout = DefaultExecTimeout
	}
	if a.Timeout < 0 {
		return fmt.Errorf("config: %s: timeout must not be negative, got %s", key, a.Timeout)
	}
	return nil
}

// validate fills the webhook defaults and enforces the transport rules.
func (w *WebhookAction) validate(key string, proxies map[string]bool) error {
	u, err := url.Parse(w.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("config: %s: url %q must be an https URL (or http to a loopback address)", key, w.URL)
	}
	if u.Scheme == "http" && !w.Insecure && !loopbackHost(u.Hostname()) {
		return fmt.Errorf("config: %s: url %q is plain http to a non-loopback host; the signed body would travel in clear, set insecure: true to allow it", key, w.URL)
	}
	if w.Secret == "" {
		return fmt.Errorf("config: %s: secret is required; store it with cloudfs config auth and reference it as keyring:<key> or secretfile:<key>", key)
	}
	if !IsSecretReference(w.Secret) {
		return fmt.Errorf("config: %s: secret must be a keyring: or secretfile: reference, not the key itself", key)
	}
	if w.Timeout == 0 {
		w.Timeout = DefaultWebhookTimeout
	}
	if w.Timeout < 0 {
		return fmt.Errorf("config: %s: timeout must not be negative, got %s", key, w.Timeout)
	}
	if w.Proxy != "" && !proxies[w.Proxy] {
		return fmt.Errorf("config: %s: references unknown proxy %q", key, w.Proxy)
	}
	return nil
}

// loopbackHost reports whether host names the local machine: "localhost"
// or a loopback IP literal. Anything that needs a DNS lookup is not.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// knownNames rejects a value outside allowed, naming the key and the list.
func knownNames(key string, values, allowed []string) error {
	for _, v := range values {
		if !hasName(allowed, v) {
			return fmt.Errorf("config: %s has unknown entry %q; choose from %s", key, v, strings.Join(allowed, ", "))
		}
	}
	return nil
}

func hasName(list []string, name string) bool {
	for _, s := range list {
		if s == name {
			return true
		}
	}
	return false
}
