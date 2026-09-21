package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
	"cloudfs/internal/mcpsrv"
)

// TokenManager is the owner-daemon slice needed by the local integration.
// Both the CLI and control panel use it so credentials and rollback semantics
// cannot drift between the two entry points.
type TokenManager interface {
	CreateToken(context.Context, agent.TokenSpec) (string, agent.Principal, error)
	VerifyToken(context.Context, string) (agent.Principal, error)
	RevokeToken(context.Context, string) (agent.Principal, error)
}

// Credential is kept only on this machine. The clear token never enters the
// skill, the mounted drive, API responses, or status output.
type Credential struct {
	Token    string `json:"token"`
	ID       string `json:"id"`
	StateDir string `json:"state_dir"`
}

func credentialPath(home, client string) string {
	return filepath.Join(home, ".config", "cloudfs", "agents", client+"-credential.json")
}

func loadCredential(home, client string) (Credential, error) {
	var cred Credential
	b, err := os.ReadFile(credentialPath(home, client))
	if errors.Is(err, os.ErrNotExist) {
		return cred, nil
	}
	if err != nil {
		return cred, err
	}
	if err := json.Unmarshal(b, &cred); err != nil {
		return cred, fmt.Errorf("%s credential: %w", client, err)
	}
	return cred, nil
}

func saveCredential(home, client string, cred Credential) error {
	p := credentialPath(home, client)
	b, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	return atomic(p, b)
}

// InstallClient installs one client as one recoverable unit. A token created
// by a failed install is revoked and its local clear-text copy removed.
func InstallClient(ctx context.Context, home, client, version, binary, url string, cfg *config.Config, tokens TokenManager) error {
	if cfg == nil || tokens == nil {
		return errors.New("agent integration requires the running owner daemon")
	}
	if url == "" {
		return errors.New("agent integration requires the owner's MCP HTTP listener")
	}
	cred, err := loadCredential(home, client)
	if err != nil {
		return err
	}
	if cred.StateDir != "" && cred.StateDir != cfg.StateDir() {
		return errors.New("existing installation belongs to another CloudFS owner; uninstall it from that owner first")
	}
	created := false
	if cred.Token == "" {
		plain, principal, err := tokens.CreateToken(ctx, agent.TokenSpec{
			Name: "integration-" + client, Read: cfg.MCP.Allow,
			ReadOnly: cfg.MCP.ReadOnly, Owner: agent.DefaultOwner(),
		})
		if err != nil {
			return err
		}
		cred = Credential{Token: plain, ID: principal.ID, StateDir: cfg.StateDir()}
		created = true
		if err := saveCredential(home, client, cred); err != nil {
			_, _ = tokens.RevokeToken(ctx, principal.ID)
			return err
		}
	} else if _, err := tokens.VerifyToken(ctx, cred.Token); err != nil {
		return fmt.Errorf("saved %s credential is invalid; uninstall then reinstall: %w", client, err)
	}
	snippet, err := mcpsrv.ClientConfigFor(mcpsrv.ClientOptions{
		Client: client, Binary: binary, Transport: "http", URL: url, Token: cred.Token,
	})
	if err == nil {
		err = Apply(home, client, version, snippet, false, cfg.SourcePath)
	}
	if err != nil && created {
		_, _ = tokens.RevokeToken(ctx, cred.ID)
		_ = os.Remove(credentialPath(home, client))
	}
	return err
}

// UninstallClient removes only unchanged managed content, then revokes the
// credential created for this integration.
func UninstallClient(ctx context.Context, home, client, version string, cfg *config.Config, tokens TokenManager) error {
	if cfg == nil || tokens == nil {
		return errors.New("agent integration requires the running owner daemon")
	}
	cred, err := loadCredential(home, client)
	if err != nil {
		return err
	}
	if cred.StateDir != "" && cred.StateDir != cfg.StateDir() {
		return errors.New("installation belongs to another CloudFS owner; uninstall it from that owner")
	}
	if err := Apply(home, client, version, "", true); err != nil {
		return err
	}
	if cred.ID != "" {
		if _, err := tokens.RevokeToken(ctx, cred.ID); err != nil {
			return err
		}
		if err := os.Remove(credentialPath(home, client)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ProbeClient validates the saved credential without exposing it.
func ProbeClient(ctx context.Context, home, client, url string) (string, string) {
	cred, err := loadCredential(home, client)
	if err != nil || cred.Token == "" || url == "" {
		return "unavailable", "unavailable"
	}
	return Probe(ctx, url, cred.Token)
}
