package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

const accountBindingPrefix = "cloudfs-account:"
const legacyBindingPrefix = "legacy-sha256:"
const effectiveBindingPrefix = "effective-v1:"

// NewAccountBinding returns an opaque local credential generation. It is not
// a cloud account identifier and must never be accepted as proof from a
// provider; it fences writes across configuration reauthorization.
func NewAccountBinding() string { return accountBindingPrefix + uuid.NewString() }

// ValidAccountBinding accepts generated bindings and deterministic bindings
// used to migrate configurations created before this field existed.
func ValidAccountBinding(value string) bool {
	if strings.HasPrefix(value, accountBindingPrefix) {
		u, err := uuid.Parse(strings.TrimPrefix(value, accountBindingPrefix))
		return err == nil && accountBindingPrefix+u.String() == value
	}
	if strings.HasPrefix(value, legacyBindingPrefix) {
		raw := strings.TrimPrefix(value, legacyBindingPrefix)
		_, err := hex.DecodeString(raw)
		return err == nil && len(raw) == sha256.Size*2 && strings.ToLower(raw) == raw
	}
	return false
}

// EffectiveAccountBinding combines a generated authorization generation with
// the account-locating, non-secret remote settings. This catches unsupported
// manual endpoint/principal edits while remaining stable across token refresh.
// A legacy configuration instead gets a compatibility generation derived from
// its unresolved configuration. TokenPersister freezes that value before its
// first automatic inline-to-reference credential migration.
func EffectiveAccountBinding(r Remote) (string, error) {
	if r.AccountBinding != "" {
		if !ValidAccountBinding(r.AccountBinding) {
			return "", &bindingError{}
		}
		if strings.HasPrefix(r.AccountBinding, legacyBindingPrefix) {
			return r.AccountBinding, nil
		}
		identity := make(map[string]any, len(r.Extra))
		for key, value := range r.Extra {
			if !IsSecretField(key) {
				identity[key] = value
			}
		}
		payload, err := json.Marshal(struct {
			Generation string
			Type       string
			Extra      map[string]any
		}{Generation: r.AccountBinding, Type: r.Type, Extra: identity})
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(payload)
		return effectiveBindingPrefix + hex.EncodeToString(sum[:]), nil
	}
	// Operational routing and throttling fields live outside this projection.
	// Extra contains endpoint, principal and unresolved credential references;
	// all can affect which account receives a write and are conservatively
	// treated as identity-bearing for a legacy configuration.
	payload, err := json.Marshal(struct {
		Type  string
		Extra map[string]any
	}{Type: r.Type, Extra: r.Extra})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return legacyBindingPrefix + hex.EncodeToString(sum[:]), nil
}

type bindingError struct{}

func (*bindingError) Error() string { return "config: invalid account binding" }
