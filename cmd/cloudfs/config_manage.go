package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"cloudfs/internal/auth"
	"cloudfs/internal/config"
	"cloudfs/internal/daemon"
	"cloudfs/internal/provider"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

type configIO struct {
	In         io.Reader
	Out, Err   io.Writer
	ReadSecret func(string) (string, error)
	OpenURL    func(context.Context, string) error
}

func configFlags(args []string) (*flags, map[string]string, error) {
	var rest []string
	sets := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a != "--set" && !strings.HasPrefix(a, "--set=") {
			rest = append(rest, a)
			continue
		}
		value := strings.TrimPrefix(a, "--set=")
		if a == "--set" {
			i++
			if i >= len(args) {
				return nil, nil, errors.New("config: --set requires key=value")
			}
			value = args[i]
		}
		key, v, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return nil, nil, errors.New("config: --set requires key=value")
		}
		if _, dup := sets[key]; dup {
			return nil, nil, fmt.Errorf("config: duplicate --set key %s", key)
		}
		sets[key] = v
	}
	return parseFlags(rest, "json", "stdin", "migrate", "check", "no-check", "no-browser"), sets, nil
}

func runConfig(ctx context.Context, args []string, c configIO) error {
	f, sets, err := configFlags(args)
	if err != nil {
		return err
	}
	action := f.arg(0)
	allowed := map[string]bool{"config": true}
	bools := map[string]bool{}
	switch action {
	case "add":
		for _, k := range []string{"type", "proxy", "pool", "mount", "prefix", "mount-root", "mode", "url", "host", "user", "username", "client-id", "root", "root-id", "drive-id", "base-url", "key-file", "known-hosts"} {
			allowed[k] = true
		}
	case "list":
		bools["json"] = true
	case "auth":
		for _, k := range []string{"field", "timeout", "redirect-uri"} {
			allowed[k] = true
		}
		for _, k := range []string{"stdin", "migrate", "check", "no-check", "no-browser"} {
			bools[k] = true
		}
	default:
		return errors.New("config: use check, add, list or auth")
	}
	for k := range f.values {
		if !allowed[k] {
			return fmt.Errorf("config %s: unknown flag --%s", action, k)
		}
	}
	for k := range f.bools {
		if !bools[k] {
			return fmt.Errorf("config %s: --%s requires a value or is unknown", action, k)
		}
	}
	if action != "add" && len(sets) > 0 {
		return errors.New("config: --set is only valid for add")
	}
	if action == "list" && len(f.args) != 1 {
		return errors.New("config list takes no remote name")
	}
	if action != "list" && (len(f.args) != 2 || f.arg(1) == "") {
		return fmt.Errorf("config %s requires one remote name", action)
	}
	path := f.str("config", defaultConfigPath())
	if action == "add" {
		return configAdd(path, f, sets, c)
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	if action == "list" {
		return configList(cfg, c.Out, f.bools["json"])
	}
	return configAuth(ctx, cfg, f, c)
}

func configAdd(path string, f *flags, sets map[string]string, c configIO) error {
	out := c.Out
	in, interactive := interactiveInput(c)
	typ := f.str("type", "")
	known := false
	for _, t := range provider.Types() {
		if typ == t {
			known = true
		}
	}
	if !known {
		if typ != "" || !interactive {
			return fmt.Errorf("config add: --type must name a registered provider (see cloudfs providers)")
		}
		chosen, err := chooseType(in, out)
		if err != nil {
			return err
		}
		typ = chosen
	}
	r := config.Remote{Type: typ, Proxy: f.str("proxy", ""), Extra: map[string]any{}}
	stringFields := map[string]bool{}
	for _, key := range []string{"url", "host", "user", "username", "client-id", "root", "root-id", "drive-id", "base-url", "key-file", "known-hosts"} {
		if value, ok := f.values[key]; ok {
			sets[strings.ReplaceAll(key, "-", "_")] = value
			stringFields[strings.ReplaceAll(key, "-", "_")] = true
		}
	}
	// Whatever the driver says it needs and the flags did not provide is asked
	// for; with no terminal, the same list is reported instead.
	if interactive {
		if err := collectFields(in, out, typ, sets); err != nil {
			return err
		}
		for key := range sets {
			stringFields[key] = true
		}
	} else if missing := missingRequired(typ, sets); len(missing) > 0 {
		return fmt.Errorf("config add: %s needs %s; pass them with --set or run this from a terminal",
			typ, strings.Join(missing, ", "))
	}
	for key, value := range sets {
		if config.IsSecretField(key) || strings.HasPrefix(key, "_") || key == "type" || key == "proxy" || key == "qps" || key == "upload_workers" {
			return fmt.Errorf("config add: %s cannot be set with --set; credentials belong in config auth", key)
		}
		if strings.HasSuffix(key, "url") || key == "url" {
			if u, err := url.Parse(value); err == nil && u.User != nil {
				return errors.New("config add: URLs must not contain credentials; use config auth")
			}
		}
		var parsed any = value
		if !stringFields[key] {
			if err := yaml.Unmarshal([]byte(value), &parsed); err != nil {
				return fmt.Errorf("config add: invalid scalar for %s", key)
			}
			switch parsed.(type) {
			case string, int, uint64, float64, bool:
			default:
				return fmt.Errorf("config add: --set %s requires a scalar value", key)
			}
		}
		r.Extra[key] = parsed
	}
	// A shipped OAuth application, where one exists, is recorded now rather
	// than resolved at authorization time: client_id is part of the account's
	// effective binding, so filling it in later would move that binding.
	daemon.FillBuiltinClientID(&r)
	opt := config.AddRemoteOptions{MountPath: f.str("mount", ""), Prefix: f.str("prefix", ""), Root: f.str("mount-root", ""), Mode: config.Mode(f.str("mode", "")), Pool: f.str("pool", "")}
	if opt.MountPath == "" && (opt.Prefix != "" || opt.Root != "" || opt.Mode != "") {
		return errors.New("config add: --prefix, --mount-root and --mode require --mount")
	}
	if err := config.AddRemote(path, f.arg(1), r, opt); err != nil {
		return err
	}
	fmt.Fprintf(out, "added remote %q (%s); run cloudfs config auth %s --config %q\n", f.arg(1), typ, f.arg(1), path)
	if opt.Pool != "" {
		fmt.Fprintf(out, "  joined pool %q; it takes effect at the daemon's next start\n", opt.Pool)
	}
	if hint := credentialHint(typ); hint != "" {
		fmt.Fprintf(out, "  it will ask for %s\n", hint)
	}
	return nil
}

type remoteSummary struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Credentials map[string]string `json:"credentials"`
}

func configList(cfg *config.Config, out io.Writer, asJSON bool) error {
	names := make([]string, 0, len(cfg.Remotes))
	for name := range cfg.Remotes {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]remoteSummary, 0, len(names))
	for _, name := range names {
		r := cfg.Remotes[name]
		row := remoteSummary{Name: name, Type: r.Type, Credentials: map[string]string{}}
		for key, v := range r.Extra {
			if config.IsSecretField(key) {
				storage := "inline"
				if value, ok := v.(string); ok {
					if value == "" {
						storage = "empty"
					} else if config.IsSecretReference(value) {
						storage, _, _ = strings.Cut(value, ":")
					}
				}
				row.Credentials[key] = storage
			}
		}
		rows = append(rows, row)
	}
	if asJSON {
		return json.NewEncoder(out).Encode(rows)
	}
	for _, row := range rows {
		keys := make([]string, 0, len(row.Credentials))
		for key := range row.Credentials {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var state []string
		for _, key := range keys {
			state = append(state, key+"="+row.Credentials[key])
		}
		if len(state) == 0 {
			state = []string{"no stored credentials"}
		}
		fmt.Fprintf(out, "%q  %s  %s\n", row.Name, row.Type, strings.Join(state, ", "))
	}
	return nil
}

func readCredentialInput(in io.Reader, field string) (map[string]string, error) {
	b, err := io.ReadAll(io.LimitReader(in, (1<<20)+1))
	if err != nil {
		return nil, errors.New("config auth: cannot read credentials from stdin")
	}
	if len(b) > 1<<20 {
		return nil, errors.New("config auth: credential input exceeds 1 MiB")
	}
	fields := map[string]string{}
	if field != "" {
		fields[field] = strings.TrimRight(string(b), "\r\n")
	} else {
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		if err := dec.Decode(&fields); err != nil {
			return nil, errors.New("config auth: expected a YAML or JSON mapping of credential names to strings")
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return nil, errors.New("config auth: expected exactly one credential document")
		}
	}
	if len(fields) == 0 {
		return nil, errors.New("config auth: no credentials supplied")
	}
	for key, value := range fields {
		if !config.IsSecretField(key) || value == "" {
			return nil, fmt.Errorf("config auth: invalid or empty credential field %q", key)
		}
	}
	return fields, nil
}

func (c configIO) secret(ctx context.Context, label string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.ReadSecret != nil {
		return c.ReadSecret(label)
	}
	f, ok := c.In.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", errors.New("config auth: interactive input requires a terminal; use --stdin for credential import")
	}
	fd := int(f.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer func() { term.Restore(fd, state); fmt.Fprintln(c.Err) }()
	fmt.Fprintf(c.Err, "%s (hidden): ", label)
	// A goroutine reads raw bytes; the loop selects between them and the
	// context, so a cancel returns promptly and the read path is portable
	// (no platform-specific poll). The reader is abandoned on cancel — the
	// terminal is restored by the deferred Restore, and the process is
	// exiting the credential prompt either way.
	type readByte struct {
		b   byte
		err error
	}
	bytes := make(chan readByte)
	go func() {
		buf := make([]byte, 1)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				bytes <- readByte{b: buf[0]}
			}
			if err != nil {
				bytes <- readByte{err: err}
				return
			}
		}
	}()
	var b []byte
	for {
		var rb readByte
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case rb = <-bytes:
		}
		if rb.err != nil {
			if rb.err == io.EOF {
				return "", io.EOF
			}
			return "", rb.err
		}
		one := [1]byte{rb.b}
		switch one[0] {
		case '\r', '\n':
			return string(b), nil
		case 3:
			return "", context.Canceled
		case 4:
			if len(b) == 0 {
				return "", io.EOF
			}
			return string(b), nil
		case 8, 127:
			if len(b) > 0 {
				_, n := utf8.DecodeLastRune(b)
				b = b[:len(b)-n]
			}
		default:
			if len(b) >= 1<<20 {
				return "", errors.New("config auth: credential exceeds 1 MiB")
			}
			b = append(b, one[0])
		}
	}
}

func configAuth(ctx context.Context, cfg *config.Config, f *flags, c configIO) error {
	name := f.arg(1)
	r, ok := cfg.Remotes[name]
	if !ok {
		return fmt.Errorf("config auth: unknown remote %q", name)
	}
	modes := 0
	for _, flag := range []string{"stdin", "migrate", "check"} {
		if f.bools[flag] {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("config auth: --stdin, --migrate and --check are mutually exclusive")
	}
	if (f.bools["migrate"] || f.bools["check"]) && (f.bools["no-check"] || f.str("field", "") != "") {
		return errors.New("config auth: invalid option for migrate/check")
	}
	if field := f.str("field", ""); field != "" && !config.IsSecretField(field) {
		return errors.New("config auth: --field must name a credential field")
	}
	timeout, err := time.ParseDuration(f.str("timeout", "5m"))
	if err != nil || timeout <= 0 {
		return errors.New("config auth: --timeout must be a positive duration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if f.bools["check"] {
		return checkConfiguredAccount(ctx, cfg, name, c.Out)
	}
	var fields map[string]string
	if f.bools["stdin"] {
		fields, err = readCredentialInput(c.In, f.str("field", ""))
	} else if !f.bools["migrate"] {
		if field := f.str("field", ""); field != "" {
			var value string
			value, err = c.secret(ctx, field)
			fields = map[string]string{field: value}
		} else if _, oauth := daemon.OAuthProfileFor(r.Type); oauth {
			fields, err = browserAuthorize(ctx, cfg, name, r, f, c)
		} else if r.Type == "pan115" {
			// The 115 device flow saves the credential in the daemon helper,
			// so there is nothing to hand to the shared save path. Report and
			// verify it as the other flows are, then stop before the save.
			if err = device115CLI(ctx, cfg, name, c); err != nil {
				return err
			}
			fmt.Fprintf(c.Out, "credentials saved for %q; restart any running daemon using this account\n", name)
			if f.bools["no-check"] {
				return nil
			}
			updated, lerr := config.Load(cfg.SourcePath)
			if lerr != nil {
				return lerr
			}
			if cerr := checkConfiguredAccount(ctx, updated, name, c.Out); cerr != nil {
				return fmt.Errorf("credentials are saved, but %w", cerr)
			}
			return nil
		} else {
			// The one credential to ask for when there is no authorization
			// server to ask instead. Every backend with an OAuth profile is
			// handled above and must not appear here: dropbox once did, and
			// the value it collected — an access token Dropbox expires in
			// hours — is exactly the account that stops working the same
			// afternoon.
			field := map[string]string{"webdav": "pass", "openlist": "pass", "sftp": "password", "quark": "cookie", "tianyi": "password", "pan123": "client_secret", "onedrive": "access_token", "smb": "password"}[r.Type]
			if field == "" {
				return errors.New("config auth: use --stdin to import this provider's credential fields, or --check to validate existing settings")
			}
			var value string
			value, err = c.secret(ctx, field)
			fields = map[string]string{field: value}
		}
	}
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var fileFallback bool
	if f.bools["migrate"] {
		fileFallback, err = config.SaveCredentialsForRemotePreservingBinding(cfg.SourcePath, name, fields, r)
	} else {
		fileFallback, err = config.SaveCredentialsForRemote(cfg.SourcePath, name, fields, r)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "credentials saved for %q; restart any running daemon using this account\n", name)
	if fileFallback {
		fmt.Fprintln(c.Err, "credential storage: private 0600 files (not system keyring)")
	}
	if f.bools["migrate"] || f.bools["no-check"] {
		return nil
	}
	updated, err := config.Load(cfg.SourcePath)
	if err != nil {
		return err
	}
	if err := checkConfiguredAccount(ctx, updated, name, c.Out); err != nil {
		return fmt.Errorf("credentials are saved, but %w", err)
	}
	return nil
}

// device115CLI drives 115's device flow from the terminal, rendering the QR
// content the daemon hands back. The daemon saves the credential; this returns
// only after it has, and carries nothing back.
func device115CLI(ctx context.Context, cfg *config.Config, name string, c configIO) error {
	wait, err := daemon.StartDevice115Flow(ctx, cfg, name,
		func(_ context.Context, content string) error {
			qr, err := qrcode.New(content, qrcode.Medium)
			if err != nil {
				return errors.New("config auth: cannot render 115 authorization QR code")
			}
			fmt.Fprintln(c.Out, "Scan this QR code with the 115 mobile app, then confirm authorization:")
			_, err = fmt.Fprint(c.Out, qr.ToSmallString(false))
			return err
		},
		func() { fmt.Fprintln(c.Out, "QR scanned; waiting for confirmation on your phone...") })
	if err != nil {
		return err
	}
	return wait(ctx)
}

func checkConfiguredAccount(ctx context.Context, cfg *config.Config, name string, out io.Writer) error {
	if err := daemon.CheckAccount(ctx, cfg, name); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Provider errors may embed signed URLs, cookies or OAuth query
		// strings. Do not print them from credential-management commands.
		return daemon.SanitizeAccountError(err)
	}
	fmt.Fprintf(out, "account %q: root listing succeeded\n", name)
	return nil
}

func browserAuthorize(ctx context.Context, cfg *config.Config, name string, r config.Remote, f *flags, c configIO) (map[string]string, error) {
	get := func(key string) string { value, _ := r.Extra[key].(string); return value }
	// The profile decides whether a secret is part of this authorization at
	// all, so it has to be read before anything asks for one. A public client
	// has none to give, and prompting for a value the app console never issued
	// is worse than having no wizard.
	profile, ok := daemon.OAuthProfileFor(r.Type)
	if !ok {
		return nil, fmt.Errorf("config auth: %q does not authorize through a browser", r.Type)
	}
	// One resolver, shared with the daemon's control-API flow: which
	// application an authorization runs as must not depend on whether it was
	// started from a terminal or from a browser. This is the path that can ask
	// for a missing secret, so it passes a prompt; the daemon passes none.
	clientID, secret, err := daemon.ResolveOAuthClient(cfg, r, profile, func(label string) (string, error) {
		return c.secret(ctx, label)
	})
	if err != nil {
		return nil, fmt.Errorf("config auth: %w", err)
	}
	client, closeHTTP, err := daemon.AuthorizationHTTP(cfg, name)
	if err != nil {
		return nil, err
	}
	defer closeHTTP()
	o := auth.OAuthOptions{Client: client, ClientID: clientID, ClientSecret: secret, RedirectURI: f.str("redirect-uri", get("oauth_redirect_uri")), RequireRefresh: true}
	if o.RedirectURI == "" {
		o.RedirectURI = "http://127.0.0.1:53682/callback"
	}
	// One table, shared with the daemon's control-API flow, so a terminal
	// login and a browser login cannot ask for different scopes.
	profile.Apply(&o, r)
	o.OpenURL = func(ctx context.Context, address string) error {
		fmt.Fprintf(c.Out, "authorize in your browser (register this callback with the app): %s\n", o.RedirectURI)
		fmt.Fprintln(c.Out, address)
		if c.OpenURL != nil {
			return c.OpenURL(ctx, address)
		}
		if f.bools["no-browser"] {
			return nil
		}
		var command string
		switch runtime.GOOS {
		case "darwin":
			command = "open"
		case "linux":
			command = "xdg-open"
		default:
			return errors.New("config auth: use --no-browser on this platform")
		}
		if err := exec.CommandContext(ctx, command, address).Run(); err != nil {
			fmt.Fprintln(c.Err, "could not open browser automatically; open the authorization URL above")
		}
		return nil
	}
	tokens, err := auth.Authorize(ctx, o)
	if err != nil {
		return nil, err
	}
	// Persist the renewable credential. Most providers let the first check
	// obtain a new access token through their refresh path; Baidu is handled
	// below because it rejects a refresh immediately after code exchange. A
	// public client has no secret to persist, and saveCredentials refuses an
	// empty value, so do not include that key unconditionally.
	saved := map[string]string{"refresh_token": tokens.RefreshToken}
	// Baidu rejects an immediate refresh after a successful code exchange as
	// "Trigger security policy". Preserve that exchange's access token so the
	// validation request does not needlessly refresh it.
	if r.Type == "baidu" {
		saved["access_token"] = tokens.AccessToken
	}
	if secret != "" {
		saved["client_secret"] = secret
	}
	return saved, nil
}
