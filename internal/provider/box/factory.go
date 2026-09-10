package box

import (
	"fmt"
	"strings"

	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

func init() {
	provider.Register("box", Factory)

	provider.RegisterFields("box", []provider.Field{
		{Name: "client_id", Prompt: "Box app client ID"},
	}, provider.Credentials{Fields: []string{"refresh_token", "client_secret"},
		Note: "the client secret from the Box developer console; config auth then opens a browser for the rest, and Box rotates the refresh token on every use"})
}

// Factory builds a Box provider from its `remotes.<name>` config block.
func Factory(name string, cfg map[string]any) (provider.Provider, error) {
	get := func(key string) string {
		if v, ok := cfg[key]; ok {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}
	client, shared := httpx.FromConfig(cfg, httpx.Options{
		Remote: name, Account: get("client_id"), UserAgent: "cloudfs/0.1",
	})
	if shared && client.UserAgent == "" {
		client.UserAgent = "cloudfs/0.1"
	}
	return New(Options{
		Name: name, APIBase: get("api_base"), UploadBase: get("upload_base"),
		TokenURL:    get("token_url"),
		AccessToken: get("access_token"), RefreshToken: get("refresh_token"),
		ClientID: get("client_id"), ClientSecret: get("client_secret"),
		Client: client,
	})
}
