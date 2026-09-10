package gdrive

import (
	"fmt"
	"strconv"
	"strings"

	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

func init() {
	provider.Register("gdrive", Factory)

	provider.RegisterFields("gdrive", []provider.Field{
		{Name: "client_id", Prompt: "OAuth client ID"},
		{Name: "drive_id", Prompt: "Shared drive ID, blank for My Drive"},
	}, provider.Credentials{Fields: []string{"refresh_token", "client_secret", "access_token"},
		Note: "the client secret from the Google Cloud console; config auth then opens a browser for the rest"})
}

// Factory builds a Drive provider from its `remotes.<name>` config block.
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
		Name: name, APIBase: get("api_base"), TokenURL: get("token_url"),
		AccessToken: get("access_token"), RefreshToken: get("refresh_token"),
		ClientID: get("client_id"), ClientSecret: get("client_secret"),
		DriveID: get("drive_id"), PartSize: partSize, Client: client,
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
		return 0, fmt.Errorf("gdrive: invalid part_size")
	}
	return n * mult, nil
}
