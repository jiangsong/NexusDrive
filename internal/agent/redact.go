package agent

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// MaxAuditArgs bounds the redacted argument object stored with an audit row.
// A row is for finding out what an agent asked, not for replaying it, so a
// call whose arguments do not fit is reduced to the names of its keys.
const MaxAuditArgs = 4 << 10

// maxTruncatedKeys bounds the key list a truncated object is reduced to.
const maxTruncatedKeys = 3 << 10

// bulkKeys name arguments that carry file content. Their strings are
// replaced by their size, which is also what bytes_in counts.
var bulkKeys = map[string]bool{"content": true, "old_text": true, "new_text": true}

// secretKeyParts are substrings of a (lower-cased) key whose value must not
// reach the audit trail at all.
var secretKeyParts = []string{"token", "cookie", "authorization", "password", "secret"}

// RedactArgs turns a tool's raw argument object into what the audit trail
// may keep and reports how many bytes of content the call carried.
//
// Strings under content, old_text and new_text become {"bytes": n} and n
// is added to the byte count; keys that look like credentials are dropped;
// any string that is a URL (or contains one) becomes "[url]", so a signed
// download link never lands in agent.db. An object that is still larger
// than MaxAuditArgs after that is replaced by {"truncated": true, "keys":
// [...]}, the sorted top-level key names cut to maxTruncatedKeys bytes. The
// result is always valid JSON: unparsable input yields {}.
func RedactArgs(raw json.RawMessage) (json.RawMessage, int64) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`), 0
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return json.RawMessage(`{}`), 0
	}
	var bytesIn int64
	v = redactValue(v, "", &bytesIn)
	out, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`), bytesIn
	}
	if len(out) <= MaxAuditArgs {
		return out, bytesIn
	}
	obj, _ := v.(map[string]any)
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kept := keys[:0]
	size := 0
	for _, k := range keys {
		size += len(k) + 3 // quotes and comma
		if size > maxTruncatedKeys {
			break
		}
		kept = append(kept, k)
	}
	out, _ = json.Marshal(map[string]any{"truncated": true, "keys": kept})
	return out, bytesIn
}

// redactValue walks v, applying the rules of RedactArgs. key is the object
// key v sits under, or "" for the root and array elements.
func redactValue(v any, key string, bytesIn *int64) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			if secretKey(k) {
				continue
			}
			out[k] = redactValue(child, k, bytesIn)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = redactValue(child, "", bytesIn)
		}
		return out
	case string:
		if bulkKeys[key] {
			*bytesIn += int64(len(x))
			return map[string]any{"bytes": len(x)}
		}
		if strings.Contains(x, "://") {
			return "[url]"
		}
		return x
	default:
		return v
	}
}

// secretKey reports whether a key names a credential.
func secretKey(k string) bool {
	lower := strings.ToLower(k)
	for _, part := range secretKeyParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}
