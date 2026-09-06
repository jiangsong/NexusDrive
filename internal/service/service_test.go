package service

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

func testConfig(mount string) *config.Config {
	return &config.Config{Mounts: []config.Mount{{Path: mount}}}
}

func TestDefinitionsQuoteArgumentsAndRestartOnFailure(t *testing.T) {
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
	if err := safeServiceValue("esc\x1bseq"); err == nil {
		t.Fatal("accepted a control character in a service argument")
	}
	if err := safeServiceValue("/opt/cloud fs/bin"); err != nil {
		t.Fatalf("rejected a legitimate path with a space: %v", err)
	}
}

func TestLinuxInstallAndUninstall(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "mount")
	if err := os.Mkdir(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	var events []string
	rt := Runtime{
		GOOS: "linux", ConfigDir: filepath.Join(root, "config"), Home: root,
		Executable: filepath.Join(root, "cloud fs"), Out: new(bytes.Buffer),
		Run: func(name string, args ...string) ([]byte, error) {
			events = append(events, name+" "+strings.Join(args, " "))
			return nil, nil
		},
		Mounted: func(string) (bool, error) { return false, nil },
		Unmount: func(string) error {
			events = append(events, "unmount")
			return nil
		},
	}
	configPath := filepath.Join(root, "config file.yaml")
	if installed, err := rt.Installed(); err != nil || installed {
		t.Fatalf("Installed before install = %v %v", installed, err)
	}
	if err := rt.Install(testConfig(mount), configPath); err != nil {
		t.Fatal(err)
	}
	file, _ := rt.File()
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), rt.Executable) || !strings.Contains(string(body), configPath) {
		t.Fatalf("installed unit: %s", body)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("unit mode: %v %v", info, err)
	}
	if len(events) != 2 || !strings.Contains(events[0], "daemon-reload") || !strings.Contains(events[1], "enable --now") {
		t.Fatalf("install commands: %v", events)
	}
	if installed, err := rt.Installed(); err != nil || !installed {
		t.Fatalf("Installed after install = %v %v", installed, err)
	}

	events = nil
	rt.Mounted = func(path string) (bool, error) {
		if path != mount {
			t.Fatalf("mount check=%q", path)
		}
		return true, nil
	}
	if err := rt.Uninstall(testConfig(mount), configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit remains: %v", err)
	}
	// The order is the invariant: stop the service, detach the mount, then
	// remove the file. A supervisor must never be able to relaunch onto a
	// mount point that was just pulled out.
	if len(events) != 3 || !strings.Contains(events[0], "disable --now") || events[1] != "unmount" || !strings.Contains(events[2], "daemon-reload") {
		t.Fatalf("uninstall order: %v", events)
	}
}

func TestLaunchdAndStatus(t *testing.T) {
	root := t.TempDir()
	var events []string
	var out bytes.Buffer
	rt := Runtime{
		GOOS: "darwin", Home: root, ConfigDir: filepath.Join(root, "config"),
		Executable: filepath.Join(root, "cloudfs"), UID: 501, Out: &out,
		Run: func(name string, args ...string) ([]byte, error) {
			events = append(events, name+" "+strings.Join(args, " "))
			if len(args) > 0 && args[0] == "print" {
				return []byte("state = running\n"), nil
			}
			return nil, nil
		},
		Mounted: func(string) (bool, error) { return false, nil },
	}
	cfg := testConfig(filepath.Join(root, "mount"))
	if err := rt.Install(cfg, filepath.Join(root, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	file, _ := rt.File()
	if len(events) != 2 || !strings.Contains(events[0], "bootout gui/501/"+Label) ||
		!strings.Contains(events[1], "bootstrap gui/501 "+file) {
		t.Fatalf("launch install: %v", events)
	}
	events, out = nil, bytes.Buffer{}
	rt.Out = &out
	if err := rt.Status(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "state = running") || len(events) != 1 ||
		!strings.Contains(events[0], "print gui/501/"+Label) {
		t.Fatalf("status output=%q events=%v", out.String(), events)
	}
	events = nil
	if err := rt.Uninstall(cfg, filepath.Join(root, "config.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plist remains: %v", err)
	}
}

func TestManagerFailureKeepsInstalledDefinition(t *testing.T) {
	root := t.TempDir()
	rt := Runtime{
		GOOS: "linux", ConfigDir: root, Home: root, Executable: "/bin/cloudfs",
		Run: func(name string, args ...string) ([]byte, error) {
			return []byte("manager unavailable"), fmt.Errorf("exit 1")
		},
		Mounted: func(string) (bool, error) { return false, nil },
	}
	err := rt.Install(testConfig(filepath.Join(root, "mount")), filepath.Join(root, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "manager unavailable") {
		t.Fatalf("manager failure=%v", err)
	}
	file, _ := rt.File()
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("recoverable definition missing: %v", err)
	}
}

func TestUnsupportedPlatform(t *testing.T) {
	rt := Runtime{GOOS: "plan9", Run: func(string, ...string) ([]byte, error) { return nil, nil }}
	if ok, why := rt.Supported(); ok || why == "" {
		t.Fatalf("Supported on plan9 = %v %q", ok, why)
	}
	if err := rt.Install(testConfig("/mnt/x"), "/tmp/c.yaml"); err == nil {
		t.Fatal("install on an unsupported platform was accepted")
	}
}

func TestDefinitionPassesNativeParser(t *testing.T) {
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
