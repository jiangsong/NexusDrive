package sftp

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

func directDial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// Factory builds an SFTP provider from config.
//
//	remotes:
//	  nas:
//	    type: sftp
//	    host: 192.168.0.20
//	    port: 22            # optional
//	    user: work          # optional, defaults to the current user
//	    key_file: ~/.ssh/id_rsa   # optional; the agent and ~/.ssh keys are tried
//	    password: ''        # optional
//	    root: ~/test        # optional, defaults to the login directory
//	    host_key_alias: nas.example   # optional; verify the key under this name
//	                                  # when dialling through a proxy/forward
func Factory(name string, cfg map[string]any) (provider.Provider, error) {
	str := func(key string) string {
		if v, ok := cfg[key]; ok {
			switch s := v.(type) {
			case string:
				return s
			case fmt.Stringer:
				return s.String()
			}
		}
		return ""
	}
	num := func(key string) int {
		if v, ok := cfg[key]; ok {
			switch n := v.(type) {
			case int:
				return n
			case int64:
				return int(n)
			case float64:
				return int(n)
			}
		}
		return 0
	}
	boolean := func(key string) bool {
		if v, ok := cfg[key]; ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
		return false
	}

	host := str("host")
	if host == "" {
		return nil, fmt.Errorf("sftp: remote %q is missing the required 'host' key", name)
	}
	// Accept "host:port" in the host field, which is what people type.
	port := num("port")
	if h, p, err := net.SplitHostPort(host); err == nil {
		host = h
		if port == 0 {
			fmt.Sscanf(p, "%d", &port)
		}
	}
	var keyFiles []string
	if kf := str("key_file"); kf != "" {
		keyFiles = append(keyFiles, expandHome(kf))
	}
	var knownHosts []string
	if kh := str("known_hosts"); kh != "" {
		knownHosts = append(knownHosts, expandHome(kh))
	}
	opt := Options{
		Name: name, Host: host, Port: port,
		User:            str("user"),
		Password:        str("password"),
		KeyFiles:        keyFiles,
		KeyPassphrase:   str("key_passphrase"),
		UseAgent:        true,
		KnownHosts:      knownHosts,
		InsecureHostKey: boolean("insecure_host_key"),
		HostKeyAlias:    str("host_key_alias"),
		// The root is a path on the server, so a leading ~ must reach the
		// server unexpanded; conn resolves it against the login directory.
		Root:              str("root"),
		PartSize:          int64(num("part_size")),
		Concurrency:       num("concurrency"),
		PacketSize:        num("packet_size"),
		Sessions:          num("connections"),
		DirectorySessions: num("directory_connections"),
		KeepAlive:         30 * time.Second,
	}
	// Route the TCP connection through the same rules as every HTTP backend,
	// and share the rate-limit buckets, when the daemon supplied them.
	if d, ok := provider.DialerFrom(cfg); ok {
		opt.Dial = dialFunc(d)
	}
	if l, ok := provider.LimitersFrom(cfg); ok {
		if reg, ok := l.(*ratelimit.Registry); ok {
			opt.Limiters = reg
		}
	}
	return New(opt)
}

// expandHome resolves a leading ~ against this machine's home directory. It
// applies only to local files such as the key and known_hosts; a remote root
// is resolved by the server.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~/"))
}

func init() {
	provider.Register("sftp", Factory)

	provider.RegisterFields("sftp", []provider.Field{
		{Name: "host", Prompt: "SSH host", Required: true, Example: "192.168.0.20"},
		{Name: "port", Prompt: "SSH port", Default: "22"},
		{Name: "user", Prompt: "SSH user, blank for the current OS user"},
		{Name: "root", Prompt: "Directory to expose", Default: "."},
		{Name: "key_file", Prompt: "Private key file, blank to use the agent or a password"},
	}, provider.Credentials{Fields: []string{"password", "key_passphrase"},
		Note: "a password, or the passphrase of the key file"})
}
