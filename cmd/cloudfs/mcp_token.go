package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/memory"
)

// runMCPToken implements `cloudfs mcp token create|list|revoke`. It works on
// agent.db directly rather than through the daemon: the store is shared
// across processes, and a running server notices a revocation by polling,
// so no daemon has to be up to manage tokens.
func runMCPToken(ctx context.Context, out io.Writer, args []string) error {
	f := parseFlags(args, "read-only", "confirm", "json")
	for k := range f.values {
		switch k {
		case "config", "name", "read", "write", "ttl":
		default:
			return fmt.Errorf("mcp token: unknown flag --%s", k)
		}
	}
	for k := range f.bools {
		switch k {
		case "read-only", "confirm", "json":
		default:
			return fmt.Errorf("mcp token: --%s requires a value or is unknown", k)
		}
	}
	action := f.arg(0)
	switch action {
	case "create", "list", "revoke":
	case "":
		return errors.New("mcp token: expected create, list or revoke")
	default:
		return fmt.Errorf("mcp token: unknown action %q", action)
	}
	if action == "revoke" && (len(f.args) != 2 || f.arg(1) == "") {
		return errors.New("mcp token revoke: give the token name or id")
	}
	if action != "revoke" && len(f.args) > 1 {
		return fmt.Errorf("mcp token %s: unexpected argument %q", action, f.arg(1))
	}
	cfg, _, err := loadConfig(f)
	if err != nil {
		return err
	}
	st, err := agent.Open(filepath.Join(cfg.Cache.Dir, "agent"))
	if err != nil {
		return err
	}
	defer st.Close()
	asJSON := f.bools["json"]
	switch action {
	case "create":
		spec, err := tokenSpecFromFlags(f)
		if err != nil {
			return err
		}
		plain, p, err := st.CreateToken(ctx, spec)
		if err != nil {
			return err
		}
		return printCreatedToken(out, plain, p, asJSON)
	case "revoke":
		if !f.bools["confirm"] {
			return fmt.Errorf("mcp token revoke: this cuts off every client using token %q; add --confirm to proceed", f.arg(1))
		}
		p, err := st.RevokeToken(ctx, f.arg(1))
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(out).Encode(p)
		}
		fmt.Fprintf(out, "token %s (%s) revoked; sessions using it close within a few seconds\n", p.Name, p.TokenPrefix)
		return nil
	}
	tokens, err := st.Tokens(ctx)
	if err != nil {
		return err
	}
	return printTokens(out, tokens, asJSON)
}

// tokenSpecFromFlags reads --name, --read, --write, --read-only and --ttl.
// Prefix lists are comma-separated like --allow. A missing --write means
// "the same as --read"; --write with an empty value means no writes.
func tokenSpecFromFlags(f *flags) (agent.TokenSpec, error) {
	spec := agent.TokenSpec{Name: f.str("name", ""), ReadOnly: f.bools["read-only"], Owner: strings.ToLower(strings.TrimSpace(f.str("owner", "")))}
	if spec.Name == "" {
		return agent.TokenSpec{}, errors.New("mcp token create: --name is required (lowercase letters, digits and dashes)")
	}
	if v := f.str("read", ""); v != "" {
		spec.Read = splitPrefixes(v)
	}
	if v, ok := f.values["write"]; ok {
		spec.Write = splitPrefixes(v)
	}
	if v := f.str("ttl", ""); v != "" && v != "never" && v != "0" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return agent.TokenSpec{}, errors.New("mcp token create: --ttl takes a duration such as 720h, or never")
		}
		spec.TTL = d
	}
	if spec.Owner != "" && !memory.ValidName(spec.Owner) {
		return agent.TokenSpec{}, errors.New("mcp token create: --owner is a name (lowercase letters, digits and dashes)")
	}
	return spec, nil
}

// splitPrefixes turns "a,b" into its non-empty parts; "" yields an empty,
// non-nil list.
func splitPrefixes(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// printCreatedToken writes the plain token exactly once. Only stdout carries
// it, so a caller that captures stdout gets the token and nothing else has
// to be scrubbed; the reminder goes to stderr.
func printCreatedToken(out io.Writer, plain string, p agent.Principal, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(struct {
			Token     string          `json:"token"`
			Principal agent.Principal `json:"principal"`
		}{plain, p})
	}
	fmt.Fprintf(out, "created token %s (fingerprint %s): %s\n", p.Name, p.TokenPrefix, scopeSummary(p.Scope))
	fmt.Fprintln(out, plain)
	fmt.Fprintln(os.Stderr, "store it now: this token cannot be shown again")
	return nil
}

func printTokens(out io.Writer, tokens []agent.Principal, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(out).Encode(tokens)
	}
	if len(tokens) == 0 {
		fmt.Fprintln(out, "no access tokens; create one with: cloudfs mcp token create --name <name> --read /path")
		return nil
	}
	now := time.Now()
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tFINGERPRINT\tSTATE\tREAD\tWRITE\tEXPIRES\tLAST USED\tID")
	for _, p := range tokens {
		expires, lastUsed := "never", "never"
		if !p.ExpiresAt.IsZero() {
			expires = p.ExpiresAt.Local().Format("2006-01-02 15:04")
		}
		if !p.LastUsedAt.IsZero() {
			lastUsed = p.LastUsedAt.Local().Format("2006-01-02 15:04")
		}
		write := "same as read"
		switch {
		case p.Scope.ReadOnly:
			write = "read-only"
		case p.Scope.Write != nil && len(p.Scope.Write) == 0:
			write = "none"
		case p.Scope.Write != nil:
			write = strings.Join(p.Scope.Write, ",")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.TokenPrefix, agent.TokenState(p, now),
			strings.Join(p.Scope.EffectiveRead(), ","), write, expires, lastUsed, p.ID)
	}
	return w.Flush()
}

// mcpTokenArgs drops the leading "token" word from the `mcp` arguments so
// runMCPToken sees "create ..." the way the tests call it. Flags before the
// word are kept, and a flag value that happens to be "token" is left alone;
// the boolean flags are the ones cmdMCP's own parse knows about.
func mcpTokenArgs(args []string) []string {
	isBool := map[string]bool{"stdio": true, "read-only": true, "confirm": true, "json": true}
	out := make([]string, 0, len(args))
	dropped := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !dropped && a == "token" {
			dropped = true
			continue
		}
		out = append(out, a)
		name := strings.TrimPrefix(a, "--")
		takesValue := strings.HasPrefix(a, "--") && !strings.Contains(name, "=") && !isBool[name]
		if takesValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}
