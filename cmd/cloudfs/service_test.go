package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"cloudfs/internal/config"
)

func serviceTestConfig(mount string) *config.Config {
	return &config.Config{Mounts: []config.Mount{{Path: mount}}}
}

func TestServiceDefinitionsQuoteArgumentsAndRestartOnFailure(t *testing.T) {
	systemd := renderSystemdUnit("/opt/cloud fs/%bin", "/tmp/config $one.yaml", "/mnt/cloud \"one\"")
	for _, want := range []string{
		"After=network-online.target",
		"RequiresMountsFor=\"/mnt/cloud \\\"one\\\"\"",
		"Restart=on-failure",
		"ExecStart=\"/opt/cloud fs/%%bin\" mount \"/mnt/cloud \\\"one\\\"\" --config \"/tmp/config $$one.yaml\" --foreground",
	} {
		if !strings.Contains(systemd, want) {
			t.Fatalf("systemd definition missing %q:\n%s", want, systemd)
		}
	}
	plist := renderLaunchdPlist("/opt/cloud&fs", "/tmp/<config>", "/mnt/\"cloud\"")
	for _, want := range []string{
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>SuccessfulExit</key>",
		"<string>/opt/cloud&amp;fs</string>",
		"<string>/tmp/&lt;config&gt;</string>",
		"<string>/mnt/&#34;cloud&#34;</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("launchd definition missing %q:\n%s", want, plist)
		}
	}
	if err := safeServiceValue("bad\npath"); err == nil {
		t.Fatal("accepted multiline service argument")
	}
}

func TestManageServiceLinuxInstallAndUninstall(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "mount")
	if err := os.Mkdir(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	var events []string
	rt := serviceRuntime{
		goos: "linux", configDir: filepath.Join(root, "config"), home: root,
		executable: filepath.Join(root, "cloud fs"), out: new(bytes.Buffer),
		run: func(name string, args ...string) ([]byte, error) {
			events = append(events, name+" "+strings.Join(args, " "))
			return nil, nil
		},
		mounted: func(string) (bool, error) { return false, nil },
		unmount: func(string) error {
			events = append(events, "unmount")
			return nil
		},
	}
	configPath := filepath.Join(root, "config file.yaml")
	if err := manageService("install", serviceTestConfig(mount), configPath, rt); err != nil {
		t.Fatal(err)
	}
	file, _ := serviceFile(rt)
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), rt.executable) || !strings.Contains(string(body), configPath) {
		t.Fatalf("installed unit: %s", body)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("unit mode: %v %v", info, err)
	}
	if len(events) != 2 || !strings.Contains(events[0], "daemon-reload") || !strings.Contains(events[1], "enable --now") {
		t.Fatalf("install commands: %v", events)
	}

	events = nil
	rt.mounted = func(path string) (bool, error) {
		if path != mount {
			t.Fatalf("mount check=%q", path)
		}
		return true, nil
	}
	if err := manageService("uninstall", serviceTestConfig(mount), configPath, rt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit remains: %v", err)
	}
	if len(events) != 3 || !strings.Contains(events[0], "disable --now") || events[1] != "unmount" || !strings.Contains(events[2], "daemon-reload") {
		t.Fatalf("uninstall order: %v", events)
	}
}

func TestManageServiceLaunchdAndStatus(t *testing.T) {
	root := t.TempDir()
	var events []string
	var out bytes.Buffer
	rt := serviceRuntime{
		goos: "darwin", home: root, configDir: filepath.Join(root, "config"),
		executable: filepath.Join(root, "cloudfs"), uid: 501, out: &out,
		run: func(name string, args ...string) ([]byte, error) {
			events = append(events, name+" "+strings.Join(args, " "))
			if len(args) > 0 && args[0] == "print" {
				return []byte("state = running\n"), nil
			}
			return nil, nil
		},
		mounted: func(string) (bool, error) { return false, nil },
	}
	cfg := serviceTestConfig(filepath.Join(root, "mount"))
	if err := manageService("install", cfg, filepath.Join(root, "config.yaml"), rt); err != nil {
		t.Fatal(err)
	}
	file, _ := serviceFile(rt)
	if len(events) != 2 || !strings.Contains(events[0], "bootout gui/501/"+serviceLabel) ||
		!strings.Contains(events[1], "bootstrap gui/501 "+file) {
		t.Fatalf("launch install: %v", events)
	}
	events, out = nil, bytes.Buffer{}
	rt.out = &out
	if err := manageService("status", nil, "", rt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "state = running") || len(events) != 1 ||
		!strings.Contains(events[0], "print gui/501/"+serviceLabel) {
		t.Fatalf("status output=%q events=%v", out.String(), events)
	}
	events = nil
	if err := manageService("uninstall", cfg, filepath.Join(root, "config.yaml"), rt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist remains: %v", err)
	}
}

func TestServiceManagerFailureKeepsInstalledDefinition(t *testing.T) {
	root := t.TempDir()
	rt := serviceRuntime{
		goos: "linux", configDir: root, home: root, executable: "/bin/cloudfs",
		run: func(name string, args ...string) ([]byte, error) {
			return []byte("manager unavailable"), fmt.Errorf("exit 1")
		},
		mounted: func(string) (bool, error) { return false, nil },
	}
	err := manageService("install", serviceTestConfig(filepath.Join(root, "mount")), filepath.Join(root, "config.yaml"), rt)
	if err == nil || !strings.Contains(err.Error(), "manager unavailable") {
		t.Fatalf("manager failure=%v", err)
	}
	file, _ := serviceFile(rt)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("recoverable definition missing: %v", err)
	}
}

func TestServiceDefinitionPassesNativeParser(t *testing.T) {
	switch runtime.GOOS {
	case "darwin":
		bin, err := exec.LookPath("plutil")
		if err != nil {
			t.Skip("plutil unavailable")
		}
		path := filepath.Join(t.TempDir(), "cloudfs.plist")
		if err := writeServiceFile(path, []byte(renderLaunchdPlist("/usr/local/bin/cloudfs", "/tmp/config.yaml", "/tmp/cloud"))); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(bin, "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("plutil: %v: %s", err, out)
		}
	case "linux":
		bin, err := exec.LookPath("systemd-analyze")
		if err != nil {
			t.Skip("systemd-analyze unavailable")
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "cloudfs.service")
		if err := writeServiceFile(path, []byte(renderSystemdUnit(executable, "/tmp/config.yaml", "/tmp/cloud"))); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(bin, "verify", path).CombinedOutput(); err != nil {
			t.Fatalf("systemd-analyze: %v: %s", err, out)
		}
	}
}
