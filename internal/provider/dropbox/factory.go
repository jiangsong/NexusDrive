package dropbox

import (
	"fmt"
	"strconv"
	"strings"

	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

func init() {
	provider.Register("dropbox", Factory)

	provider.RegisterFields("dropbox", []provider.Field{
		{Name: "client_id", Prompt: "App key"},
	}, provider.Credentials{Fields: []string{"refresh_token", "access_token", "client_secret"},
		Note: "a refresh token from the Dropbox app console; an access token alone expires in hours"})
}

func Factory(name string, cfg map[string]any) (provider.Provider, error) {
	get := func(key string) string {
		if v, ok := cfg[key]; ok {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}
	partSize, err := parseSize(get("part_size"))
	if err != nil {
		return nil, err
	}
	client, shared := httpx.FromConfig(cfg, httpx.Options{
		Remote: name, Account: get("client_id"), UserAgent: "cloudfs/0.1",
	})
	if shared && client.UserAgent == "" {
		client.UserAgent = "cloudfs/0.1"
	}
	return New(Options{
		Name: name, APIBase: get("api_base"), ContentBase: get("content_base"), OAuthURL: get("oauth_url"),
		AccessToken: get("access_token"), RefreshToken: get("refresh_token"),
		ClientID: get("client_id"), ClientSecret: get("client_secret"),
		PartSize: partSize, Client: client,
	})
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
		return 0, fmt.Errorf("dropbox: invalid part_size")
	}
	return n * mult, nil
}
