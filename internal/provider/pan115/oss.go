package pan115

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"cloudfs/internal/provider"
)

// ossCreds are the temporary STS credentials 115 hands out for the OSS bucket
// that backs a non-rapid upload.
//
// UNVERIFIED: /open/upload/get_token is documented with capitalised keys
// (AccessKeyId / AccessKeySecret / SecurityToken / expiration / endpoint).
// Lower-case aliases are also accepted below so a casing change does not
// silently produce an unsigned request.
type ossCreds struct {
	AccessKeyID     string `json:"AccessKeyId"`
	AccessKeySecret string `json:"AccessKeySecret"`
	SecurityToken   string `json:"SecurityToken"`
	Expiration      string `json:"expiration"`
	Endpoint        string `json:"endpoint"`

	AccessKeyIDAlt     string `json:"access_key_id"`
	AccessKeySecretAlt string `json:"access_key_secret"`
	SecurityTokenAlt   string `json:"security_token"`
	EndpointAlt        string `json:"endpoint_url"`
}

// normalise folds the alias fields into the canonical ones.
func (c *ossCreds) normalise() {
	c.AccessKeyID = firstNonEmpty(c.AccessKeyID, c.AccessKeyIDAlt)
	c.AccessKeySecret = firstNonEmpty(c.AccessKeySecret, c.AccessKeySecretAlt)
	c.SecurityToken = firstNonEmpty(c.SecurityToken, c.SecurityTokenAlt)
	c.Endpoint = firstNonEmpty(c.Endpoint, c.EndpointAlt)
}

func (c *ossCreds) valid() bool {
	return c.AccessKeyID != "" && c.AccessKeySecret != ""
}

// ossURL builds the request URL for bucket/object.
//
// OSS is normally addressed virtual-host style (https://bucket.endpoint/key),
// but an endpoint that already carries a scheme is used path-style
// (scheme://host/bucket/key). That is what lets a test — or a corporate OSS
// gateway — point the driver at an arbitrary origin.
func ossURL(endpoint, bucket, object string, query url.Values) string {
	var base string
	if strings.Contains(endpoint, "://") {
		base = strings.TrimRight(endpoint, "/") + "/" + bucket + "/" + object
	} else {
		base = "https://" + bucket + "." + strings.TrimRight(endpoint, "/") + "/" + object
	}
	if len(query) == 0 {
		return base
	}
	return base + "?" + encodeOSSQuery(query)
}

// encodeOSSQuery encodes sub-resources in sorted order. Valueless
// sub-resources such as "uploads" must be emitted without "=".
func encodeOSSQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := q.Get(k)
		if v == "" {
			parts = append(parts, url.QueryEscape(k))
			continue
		}
		parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
	}
	return strings.Join(parts, "&")
}

// ossStringToSign builds the OSS V1 (HMAC-SHA1) string to sign:
//
//	VERB\nContent-MD5\nContent-Type\nDate\nCanonicalizedOSSHeaders\nCanonicalizedResource
//
// CanonicalizedOSSHeaders is every x-oss-* header, lower-cased and sorted, one
// "k:v\n" per line; CanonicalizedResource is /bucket/object plus the sorted
// sub-resources. Getting either wrong yields a SignatureDoesNotMatch, so this
// function is unit-tested against a hand-computed vector.
func ossStringToSign(method, contentMD5, contentType, date string, headers http.Header, bucket, object string, query url.Values) string {
	var canonHeaders strings.Builder
	keys := make([]string, 0, len(headers))
	for k := range headers {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-oss-") {
			keys = append(keys, lk)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&canonHeaders, "%s:%s\n", k, strings.TrimSpace(headers.Get(k)))
	}
	resource := "/" + bucket + "/" + object
	if len(query) > 0 {
		resource += "?" + encodeOSSSubResource(query)
	}
	return strings.Join([]string{method, contentMD5, contentType, date}, "\n") +
		"\n" + canonHeaders.String() + resource
}

// encodeOSSSubResource renders sub-resources for the canonical resource. They
// are sorted and, unlike the query string, are not percent-encoded.
func encodeOSSSubResource(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if v := q.Get(k); v != "" {
			parts = append(parts, k+"="+v)
			continue
		}
		parts = append(parts, k)
	}
	return strings.Join(parts, "&")
}

// ossAuthorization signs stringToSign with the OSS access key secret.
func ossAuthorization(keyID, secret, stringToSign string) string {
	m := hmac.New(sha1.New, []byte(secret))
	m.Write([]byte(stringToSign))
	return "OSS " + keyID + ":" + base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// initiateMultipartUploadResult is the XML answer to POST ?uploads.
type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// completeMultipartUpload is the XML body of the completion request. OSS
// requires the parts in ascending PartNumber order.
type completeMultipartUpload struct {
	XMLName xml.Name  `xml:"CompleteMultipartUpload"`
	Parts   []ossPart `xml:"Part"`
}

// ossPart is one entry of completeMultipartUpload.
type ossPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// isLinkExpired reports whether err is the CDN telling us the URL died.
func isLinkExpired(err error) bool {
	return errors.Is(err, provider.ErrLinkExpired)
}
