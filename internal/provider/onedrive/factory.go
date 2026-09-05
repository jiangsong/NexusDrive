package onedrive

import (
	"fmt"
	"strconv"
	"strings"

	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"
)

func init() { provider.Register("onedrive", Factory) }

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
	tokenURL := get("token_url")
	if tokenURL == "" {
		tenant := get("tenant")
		if tenant == "" {
			tenant = "common"
		}
		if strings.ContainsAny(tenant, "/\\?&#\r\n\x00") {
			return nil, errorsInvalid("tenant")
		}
		tokenURL = "https://login.microsoftonline.com/" + tenant + "/oauth2/v2.0/token"
	}
	client, shared := httpx.FromConfig(cfg, httpx.Options{Remote: name, Account: get("client_id"), UserAgent: "cloudfs/0.1"})
	if shared && client.UserAgent == "" {
		client.UserAgent = "cloudfs/0.1"
	}
	return New(Options{
		Name: name, GraphBase: get("graph_base"), DriveID: get("drive_id"), TokenURL: tokenURL,
		AccessToken: get("access_token"), RefreshToken: get("refresh_token"),
		ClientID: get("client_id"), ClientSecret: get("client_secret"), Scope: get("scope"),
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
		return 0, errorsInvalid("part_size")
	}
	return n * mult, nil
}

func errorsInvalid(field string) error { return fmt.Errorf("onedrive: invalid %s", field) }
