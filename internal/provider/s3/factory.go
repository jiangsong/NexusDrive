package s3

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"cloudfs/internal/net/ratelimit"
	"cloudfs/internal/provider"
	"cloudfs/internal/provider/httpx"

	"github.com/minio/minio-go/v7"
)

func init() { provider.Register("s3", Factory) }

// Factory builds an S3/S3-compatible provider. Credentials are resolved from
// the project's secret store before this function is called.
func Factory(name string, cfg map[string]any) (provider.Provider, error) {
	str := func(key string) string {
		if v, ok := cfg[key]; ok {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return ""
	}
	boolean := func(key string) bool {
		v, _ := cfg[key].(bool)
		return v
	}
	endpoint := str("endpoint")
	if endpoint == "" {
		endpoint = "https://s3.amazonaws.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || (u.Path != "" && u.Path != "/") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("s3: endpoint must be an http(s) origin without path, credentials, query, or fragment")
	}
	accessKey := str("access_key_id")
	if accessKey == "" {
		accessKey = str("access_key")
	}
	secretKey := str("secret_access_key")
	if boolean("anonymous") && (accessKey != "" || secretKey != "" || str("session_token") != "") {
		return nil, fmt.Errorf("s3: anonymous cannot be combined with credentials")
	}
	if !boolean("anonymous") && (accessKey == "" || secretKey == "") {
		return nil, fmt.Errorf("s3: access_key_id and secret_access_key are required unless anonymous is true")
	}
	if (accessKey == "") != (secretKey == "") {
		return nil, fmt.Errorf("s3: access_key_id and secret_access_key must be supplied together")
	}
	for _, credential := range []string{accessKey, secretKey, str("session_token")} {
		if len(credential) > 4096 || strings.ContainsAny(credential, "\r\n\x00") {
			return nil, fmt.Errorf("s3: credential contains an unsafe character or is too large")
		}
	}
	lookup := minio.BucketLookupAuto
	switch strings.ToLower(str("lookup")) {
	case "", "auto":
	case "path":
		lookup = minio.BucketLookupPath
	case "dns", "virtual-host":
		lookup = minio.BucketLookupDNS
	default:
		return nil, fmt.Errorf("s3: lookup must be auto, path, or dns")
	}
	partSize, err := byteSize(str("part_size"))
	if err != nil {
		return nil, err
	}
	httpClient, _ := httpx.FromConfig(cfg, httpx.Options{})
	return New(Options{
		Name: name, Endpoint: u, Region: str("region"), Bucket: str("bucket"), Prefix: str("prefix"),
		AccessKey: accessKey, SecretKey: secretKey, SessionToken: str("session_token"),
		Anonymous: boolean("anonymous"), Lookup: lookup, PartSize: partSize,
		Transport: httpClient.RawTransport(classifyRequest),
	})
}

func classifyRequest(req *http.Request) ratelimit.Class {
	switch req.Method {
	case http.MethodHead, http.MethodDelete:
		return ratelimit.Meta
	case http.MethodGet:
		if req.Header.Get("Range") != "" || (req.URL.RawQuery == "" && req.URL.Path != "") {
			return ratelimit.Download
		}
		return ratelimit.Meta
	case http.MethodPut, http.MethodPost:
		return ratelimit.Upload
	default:
		return ratelimit.Meta
	}
}

func byteSize(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	raw = strings.TrimSpace(strings.ToLower(raw))
	mult := int64(1)
	for suffix, value := range map[string]int64{
		"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30,
		"kb": 1000, "mb": 1000 * 1000, "gb": 1000 * 1000 * 1000,
	} {
		if strings.HasSuffix(raw, suffix) {
			raw = strings.TrimSpace(strings.TrimSuffix(raw, suffix))
			mult = value
			break
		}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || n > (1<<63-1)/mult {
		return 0, fmt.Errorf("s3: invalid part_size")
	}
	return n * mult, nil
}
