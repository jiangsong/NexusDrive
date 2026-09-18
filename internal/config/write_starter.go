package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteStarter creates the smallest configuration a control plane can be
// served from, so that adding drives can happen in the browser instead of in
// an editor.
//
// This exists because of a circle: nothing serves the control plane until a
// configuration exists, the daemon refuses a configuration that declares no
// mounts, and a mount cannot name a remote that has not been added yet — which
// left a first-time person reading a design document to hand-write YAML before
// anything in the product could help them.
//
// It deliberately declares no mounts and no remotes. A placeholder mount would
// be worse than none: `cloudfs mount` takes the first mount in the file, so a
// placeholder becomes the real mount point permanently, and the daemon refuses
// a mount whose layout is empty anyway. The setup flow writes the single real
// mount when the pool is created.
func WriteStarter(path string) error {
	// Load and editConfig both expand a leading ~, so this has to as well:
	// CLOUDFS_CONFIG set in a launchd plist or a systemd unit reaches the
	// process unexpanded, and writing the file literally would put it in a
	// directory named "~" that nothing else in the package would ever find.
	path = ExpandHome(path)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config: %s already exists; edit it, or point --config somewhere else", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	d := Default()
	cacheDir := filepath.Join(filepath.Dir(path), "cache")
	body := fmt.Sprintf(`# CloudFS 配置。这份文件由 cloudfs setup 生成，只包含起步所需的部分。
# 网盘账号、存储池和挂载点在控制台里添加，也可以用 cloudfs config add / cloudfs pool create。

cache:
  dir: %q

control:
  socket: %q
  metrics: %q
  ui: true

# 账号加进来之前这里是空的。
remotes: {}

# 挂载点在创建存储池时写入（cloudfs pool create --mount ...）。
# 这里留空是有意的：cloudfs mount 用的是第一个挂载点，占位项会永远变成错的那一个。
mounts: []
`, cacheDir, d.Control.Socket, starterMetricsAddr)
	if err := atomicPrivateWrite(path, []byte(body)); err != nil {
		return err
	}
	// A file the daemon cannot read is not a starting point. Parsing it back
	// turns a template mistake into a failure here rather than into a puzzle
	// the person meets on their next command.
	if _, err := Load(path); err != nil {
		os.Remove(path)
		return fmt.Errorf("config: the starter file did not validate: %w", err)
	}
	return nil
}

// starterMetricsAddr is the loopback address the control plane and its web UI
// answer on. It is written explicitly rather than left to a default so the
// setup flow can print a URL that is certainly the one being served.
const starterMetricsAddr = "127.0.0.1:9101"
