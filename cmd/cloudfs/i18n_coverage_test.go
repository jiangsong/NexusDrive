package main

import (
	"testing"

	"cloudfs/internal/i18n"
	"cloudfs/internal/provider"
)

// Every driver is linked into this binary, so this is the one place that can
// see the whole registry. A backend whose prompts are missing from the catalog
// still works — the driver's own prompt is the fallback — but it shows one
// language on a page rendered in the other, which is the failure this test
// exists to make loud at build time rather than at a user's first drive.
func TestEveryBackendPromptIsInTheCatalog(t *testing.T) {
	for _, typ := range provider.DescribedTypes() {
		for _, f := range provider.Fields(typ) {
			key := "field." + typ + "." + f.Name
			if !i18n.Has(key) {
				t.Errorf("backend %q field %q has no catalog entry %q", typ, f.Name, key)
			}
			for _, lang := range i18n.Supported {
				if got := i18n.T(lang, key); got == key {
					t.Errorf("catalog entry %q is missing for %q", key, lang)
				}
			}
		}
		if note := provider.CredentialsFor(typ).Note; note != "" {
			key := "creds." + typ + ".note"
			if !i18n.Has(key) {
				t.Errorf("backend %q has a credential note but no catalog entry %q", typ, key)
			}
		}
	}
}

// TestPromptsRenderInBothLanguages: a prompt that renders the same string in
// both languages is almost always a table entry copied without translating.
func TestPromptsRenderInBothLanguages(t *testing.T) {
	// Names, protocol words and identifiers are the same in both languages;
	// only these are allowed to match.
	same := map[string]bool{
		"field.s3.bucket": true, "field.s3.region": true, "field.s3.access_key_id": true,
		"field.gdrive.client_id": true, "field.dropbox.client_id": true,
		"field.aliyun.client_id": true, "field.pan123.client_id": true,
		"field.onedrive.drive_id": true, "field.smb.port": true, "field.sftp.port": true,
	}
	for _, typ := range provider.DescribedTypes() {
		for _, f := range provider.Fields(typ) {
			key := "field." + typ + "." + f.Name
			if same[key] {
				continue
			}
			if i18n.T(i18n.EN, key) == i18n.T(i18n.ZH, key) {
				t.Errorf("%s renders identically in both languages", key)
			}
		}
	}
}
