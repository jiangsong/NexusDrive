package smb

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
)

func init() {
	provider.Register("smb", Factory)

	provider.RegisterFields("smb", []provider.Field{
		{Name: "host", Prompt: "SMB server", Required: true, Example: "192.168.0.30"},
		{Name: "port", Prompt: "Port", Default: "445"},
		{Name: "share", Prompt: "Share name", Required: true},
		{Name: "user", Prompt: "User", Required: true},
		{Name: "domain", Prompt: "Domain or workgroup"},
		{Name: "root", Prompt: "Directory inside the share to expose"},
	}, provider.Credentials{Fields: []string{"password", "ntlm_hash"},
		Note: "the account password, or its NTLM hash"})
}

// Factory builds an SMB provider from its `remotes.<name>` config block.
//
// SMB does not speak HTTP, so it cannot inherit proxy routing and rate
// limiting through the shared HTTP client the way the cloud drives do. The
// daemon hands both over directly instead, and a remote configured with a
// proxy must go through that dialler or it would quietly bypass the rule set
// that governs every other egress in the process.
func Factory(name string, cfg map[string]any) (provider.Provider, error) {
	get := func(key string) string {
		if v, ok := cfg[key]; ok {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}
	port := 0
	if raw := get("port"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 || v > 65535 {
			return nil, fmt.Errorf("smb: invalid port %q", raw)
		}
		port = v
	}
	partSize, err := parseSize(get("part_size"))
	if err != nil {
		return nil, err
	}
	concurrency := 0
	if raw := get("concurrency"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 || v > 256 {
			return nil, fmt.Errorf("smb: invalid concurrency %q", raw)
		}
		concurrency = v
	}
	idle := time.Duration(0)
	if raw := get("idle_timeout"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("smb: invalid idle_timeout %q", raw)
		}
		idle = d
	}
	// ntlm_hash is password-equivalent, so it is named explicitly enough for
	// config.IsSecretField to keep it out of the YAML alongside password.
	var hash []byte
	if raw := get("ntlm_hash"); raw != "" {
		decoded, err := hex.DecodeString(raw)
		if err != nil || len(decoded) != 16 {
			return nil, fmt.Errorf("smb: ntlm_hash must be a 16-byte hex NTLM hash")
		}
		hash = decoded
	}
	host := get("host")
	if h, p, ok := splitHostPort(host); ok {
		if port != 0 && port != p {
			return nil, fmt.Errorf("smb: host carries port %d but port is set to %d", p, port)
		}
		host, port = h, p
	}
	opt := Options{
		Name: name, Host: host, Port: port, Share: get("share"),
		User: get("user"), Password: get("password"), Domain: get("domain"), Hash: hash,
		Root: get("root"), PartSize: partSize, Concurrency: concurrency, IdleTimeout: idle,
	}
	if dial, ok := provider.DialerFrom(cfg); ok {
		opt.Dial = dial
	}
	if v, ok := provider.LimitersFrom(cfg); ok {
		if reg, ok := v.(*ratelimit.Registry); ok {
			opt.Limiters = reg
		}
	}
	return New(opt)
}

// splitHostPort accepts "server:445" in the host field, which is how a user
// who has seen the sftp remote's syntax will try to write it.
func splitHostPort(raw string) (string, int, bool) {
	idx := strings.LastIndex(raw, ":")
	if idx <= 0 || strings.Contains(raw[:idx], ":") {
		return "", 0, false // no port, or a bare IPv6 literal
	}
	port, err := strconv.Atoi(raw[idx+1:])
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, false
	}
	return raw[:idx], port, true
}

func parseSize(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	raw = strings.ToLower(strings.TrimSpace(raw))
	mult := int64(1)
	for _, unit := range []struct {
		suffix string
		value  int64
	}{{"mib", 1 << 20}, {"kib", 1 << 10}, {"mb", 1_000_000}, {"kb", 1_000}} {
		if strings.HasSuffix(raw, unit.suffix) {
			raw = strings.TrimSpace(strings.TrimSuffix(raw, unit.suffix))
			mult = unit.value
			break
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || n > (1<<63-1)/mult {
		return 0, fmt.Errorf("smb: invalid part_size")
	}
	return n * mult, nil
}
