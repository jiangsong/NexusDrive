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
		Note: "the client secret from the Google Cloud console; the console or config auth asks for it, then opens a browser for the rest",
		// Google classes the scope a mounted drive needs as restricted, so no
		// application can be shipped with this binary: carrying it would mean
		// brand verification plus a third-party security assessment renewed
		// every twelve months, and every account authorized under it would
		// stop refreshing the day that lapsed. Registering one takes a few
		// minutes and the account then belongs to the person who made it.
		Setup: []string{
			"Create a project in the Google Cloud console.",
			"Enable the Google Drive API for it.",
			// A Web application client accepts only the exact redirect URIs
			// registered for it, and the callback here is a loopback port. A
			// Desktop client accepts any loopback port, so choosing it here
			// removes a step that otherwise fails with redirect_uri_mismatch.
			"Create an OAuth client and choose the Desktop app type; it accepts the loopback callback with no URI to register.",
			// An application left in Testing issues refresh tokens that expire
			// seven days after consent, and the only symptom is invalid_grant
			// on a drive that worked all week.
			"Publish the application to Production. In Testing, authorization expires after 7 days. Production does not require verification: the consent screen warns that the app is unverified, and continuing is fine for an application only you use.",
			"Paste the client ID here.",
			"Paste the client secret when the authorization step asks for it; it is stored in the system keyring, never in the configuration file.",
		}})
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
