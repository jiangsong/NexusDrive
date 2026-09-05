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
	"strings"

	"cloudfs/internal/config"
)

const serviceLabel = "io.cloudfs.mount"

type serviceRuntime struct {
	goos       string
	home       string
	configDir  string
	executable string
	uid        int
	out        io.Writer
	run        func(string, ...string) ([]byte, error)
	mounted    func(string) (bool, error)
	unmount    func(string) error
}

func realServiceRuntime() (serviceRuntime, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return serviceRuntime{}, err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return serviceRuntime{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return serviceRuntime{}, err
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return serviceRuntime{
		goos: runtime.GOOS, home: home, configDir: configDir, executable: executable,
		uid: currentUID(), out: os.Stdout,
		run: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		},
		mounted: mountpointMounted,
		unmount: unmountPath,
	}, nil
}

func cmdService(args []string) error {
	f := parseFlags(args)
	action := f.arg(0)
	if (action != "install" && action != "uninstall" && action != "status") || f.arg(1) != "" {
		return errors.New("service: expected install, uninstall or status")
	}
	rt, err := realServiceRuntime()
	if err != nil {
		return err
	}
	if action == "status" {
		return manageService(action, nil, "", rt)
	}
	cfg, configPath, err := loadConfig(f)
	if err != nil {
		return err
	}
	return manageService(action, cfg, configPath, rt)
}

func serviceFile(rt serviceRuntime) (string, error) {
	switch rt.goos {
	case "linux":
		return filepath.Join(rt.configDir, "systemd", "user", "cloudfs.service"), nil
	case "darwin":
		return filepath.Join(rt.home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
	default:
		return "", fmt.Errorf("service: %s is unsupported; use a native service manager", rt.goos)
	}
}

func manageService(action string, cfg *config.Config, configPath string, rt serviceRuntime) error {
	if action != "install" && action != "uninstall" && action != "status" {
		return errors.New("service: unknown management action")
	}
	file, err := serviceFile(rt)
	if err != nil {
		return err
	}
	if rt.out == nil {
		rt.out = io.Discard
	}
	if rt.run == nil {
		return errors.New("service: command runner is unavailable")
	}
	if action == "status" {
		var output []byte
		switch rt.goos {
		case "linux":
			output, err = rt.run("systemctl", "--user", "status", "--no-pager", "cloudfs.service")
		case "darwin":
			output, err = rt.run("launchctl", "print", fmt.Sprintf("gui/%d/%s", rt.uid, serviceLabel))
		}
		if len(output) > 0 {
			_, _ = rt.out.Write(output)
		}
		if err != nil {
			return fmt.Errorf("service: status (%s): %w", file, err)
		}
		return nil
	}
	if cfg == nil || len(cfg.Mounts) == 0 {
		return errors.New("service: a config with at least one mount is required")
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return err
	}
	mountPath, err := filepath.Abs(cfg.Mounts[0].Path)
	if err != nil {
		return err
	}
	for _, value := range []string{rt.executable, configPath, mountPath} {
		if err := safeServiceValue(value); err != nil {
			return err
		}
	}
	if action == "install" {
		var body []byte
		switch rt.goos {
		case "linux":
			body = []byte(renderSystemdUnit(rt.executable, configPath, mountPath))
		case "darwin":
			body = []byte(renderLaunchdPlist(rt.executable, configPath, mountPath))
		}
		if err := writeServiceFile(file, body); err != nil {
			return err
		}
		switch rt.goos {
		case "linux":
			if out, err := rt.run("systemctl", "--user", "daemon-reload"); err != nil {
				return serviceCommandError("reload systemd", out, err)
			}
			if out, err := rt.run("systemctl", "--user", "enable", "--now", "cloudfs.service"); err != nil {
				return serviceCommandError("enable systemd service", out, err)
			}
		case "darwin":
			domain := fmt.Sprintf("gui/%d", rt.uid)
			_, _ = rt.run("launchctl", "bootout", domain+"/"+serviceLabel)
			if out, err := rt.run("launchctl", "bootstrap", domain, file); err != nil {
				return serviceCommandError("bootstrap launchd service", out, err)
			}
		}
		fmt.Fprintf(rt.out, "installed and started %s\n", file)
		return nil
	}

	_, statErr := os.Stat(file)
	installed := statErr == nil
	switch rt.goos {
	case "linux":
		if out, err := rt.run("systemctl", "--user", "disable", "--now", "cloudfs.service"); err != nil && installed {
			return serviceCommandError("disable systemd service", out, err)
		}
	case "darwin":
		domain := fmt.Sprintf("gui/%d", rt.uid)
		if _, activeErr := rt.run("launchctl", "print", domain+"/"+serviceLabel); activeErr == nil {
			if out, err := rt.run("launchctl", "bootout", domain+"/"+serviceLabel); err != nil {
				return serviceCommandError("bootout launchd service", out, err)
			}
		}
	}
	if rt.mounted != nil {
		mounted, err := rt.mounted(mountPath)
		if err != nil {
			return fmt.Errorf("service: inspect mount point: %w", err)
		}
		if mounted {
			if rt.unmount == nil {
				return errors.New("service: mount remains but no unmount helper is available")
			}
			if err := rt.unmount(mountPath); err != nil {
				return fmt.Errorf("service: detach remaining mount before uninstall: %w", err)
			}
		}
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service: remove %s: %w", file, err)
	}
	if rt.goos == "linux" {
		if out, err := rt.run("systemctl", "--user", "daemon-reload"); err != nil {
			return serviceCommandError("reload systemd", out, err)
		}
	}
	fmt.Fprintf(rt.out, "uninstalled %s\n", file)
	return nil
}

func safeServiceValue(value string) error {
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("service: executable, config and mount paths must be nonempty single-line values")
	}
	return nil
}

func systemdQuote(value string) string {
	value = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "$$", "%", "%%").Replace(value)
	return "\"" + value + "\""
}

func renderSystemdUnit(executable, configPath, mountPath string) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=CloudFS user mount",
		"Wants=network-online.target",
		"After=network-online.target",
		"RequiresMountsFor=" + systemdQuote(mountPath),
		"",
		"[Service]",
		"Type=simple",
		"ExecStart=" + systemdQuote(executable) + " mount " + systemdQuote(mountPath) + " --config " + systemdQuote(configPath) + " --foreground",
		"Restart=on-failure",
		"RestartSec=5s",
		"TimeoutStopSec=30s",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}, "\n")
}

func xmlText(value string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

func renderLaunchdPlist(executable, configPath, mountPath string) string {
	args := []string{executable, "mount", mountPath, "--config", configPath, "--foreground"}
	var arguments strings.Builder
	for _, arg := range args {
		arguments.WriteString("\n      <string>")
		arguments.WriteString(xmlText(arg))
		arguments.WriteString("</string>")
	}
	return strings.Join([]string{
		"<?xml version=\"1.0\" encoding=\"UTF-8\"?>",
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">",
		"<plist version=\"1.0\">",
		"<dict>",
		"  <key>Label</key>",
		"  <string>" + serviceLabel + "</string>",
		"  <key>ProgramArguments</key>",
		"  <array>" + arguments.String(),
		"  </array>",
		"  <key>RunAtLoad</key>",
		"  <true/>",
		"  <key>KeepAlive</key>",
		"  <dict>",
		"    <key>SuccessfulExit</key>",
		"    <false/>",
		"  </dict>",
		"  <key>ProcessType</key>",
		"  <string>Interactive</string>",
		"</dict>",
		"</plist>",
		"",
	}, "\n")
}

func writeServiceFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("service: create directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".cloudfs-service-*")
	if err != nil {
		return fmt.Errorf("service: create temporary file: %w", err)
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := temp.Write(body); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("service: install %s: %w", path, err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	ok = true
	return nil
}

func serviceCommandError(action string, output []byte, err error) error {
	message := strings.TrimSpace(string(output))
	if len(message) > 1024 {
		message = message[:1024]
	}
	if message == "" {
		return fmt.Errorf("service: %s: %w", action, err)
	}
	return fmt.Errorf("service: %s: %w: %s", action, err, message)
}
