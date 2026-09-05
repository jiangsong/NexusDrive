package provider

import (
	"sort"
	"sync"
)

// Field describes one configuration key a backend needs, so a front end can
// ask for it without knowing which backend it is talking about.
//
// The alternative is a table of questions in the CLI keyed by provider name,
// which is the same special-casing the capability matrix exists to avoid: it
// goes stale the moment a driver changes, and a new driver is invisible to it
// until someone remembers to edit the other package.
type Field struct {
	// Name is the config key under `remotes.<name>`.
	Name string
	// Prompt is the question to put to a person, without a trailing colon.
	Prompt string
	// Required means the backend cannot be built without it.
	Required bool
	// Default is used when the answer is empty. Never a credential.
	Default string
	// Example is shown when there is no default and the shape is not obvious.
	Example string
}

// Credentials lists the credential keys a backend accepts, in the order a
// person would be asked for them. They are deliberately not part of Field:
// credentials never go into the YAML, and the flow that collects them is
// `config auth`, which stores them through the secrets layer.
type Credentials struct {
	// Fields are the credential keys, most preferred first. The first one is
	// what an interactive `config auth` asks for when no --field is given.
	Fields []string
	// Note is one line explaining how to obtain them, shown after adding a
	// remote. Empty when the flow is a browser or device authorization the
	// command drives itself.
	Note string
}

var (
	fieldsMu     sync.RWMutex
	fieldsByType = map[string][]Field{}
	credsByType  = map[string]Credentials{}
)

// RegisterFields records the public configuration a backend type needs.
// Drivers call it from init, beside Register, so the description lives with
// the code that consumes it.
func RegisterFields(typ string, fields []Field, creds Credentials) {
	fieldsMu.Lock()
	defer fieldsMu.Unlock()
	fieldsByType[typ] = fields
	credsByType[typ] = creds
}

// Fields returns the public configuration for a backend type, or nil when the
// driver has not described itself. A caller must treat nil as "ask nothing and
// let the driver's own validation report what is missing", not as "no fields".
func Fields(typ string) []Field {
	fieldsMu.RLock()
	defer fieldsMu.RUnlock()
	out := make([]Field, len(fieldsByType[typ]))
	copy(out, fieldsByType[typ])
	return out
}

// CredentialsFor returns the credential keys a backend type accepts.
func CredentialsFor(typ string) Credentials {
	fieldsMu.RLock()
	defer fieldsMu.RUnlock()
	c := credsByType[typ]
	out := Credentials{Note: c.Note, Fields: make([]string, len(c.Fields))}
	copy(out.Fields, c.Fields)
	return out
}

// DescribedTypes lists the backend types that have described themselves,
// sorted. It is what an interactive front end offers.
func DescribedTypes() []string {
	fieldsMu.RLock()
	defer fieldsMu.RUnlock()
	out := make([]string, 0, len(fieldsByType))
	for typ := range fieldsByType {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}
