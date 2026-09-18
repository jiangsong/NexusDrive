package config

import "testing"

// The OAuth application fields are stored like credentials — the secret half
// belongs in the keyring, never in the file — but they do not say that an
// account is authorized. A person who has registered an application and has
// not yet signed in has an account with a secret and no access to anything.
func TestOAuthApplicationFieldsAreNotAnAccountCredential(t *testing.T) {
	for _, f := range []string{"client_id", "client_secret"} {
		if !IsOAuthAppField(f) {
			t.Fatalf("%s is part of the application registration", f)
		}
	}
	for _, f := range []string{"refresh_token", "access_token", "cookie", "password"} {
		if IsOAuthAppField(f) {
			t.Fatalf("%s identifies the account, not the application", f)
		}
	}
	// The secret half still travels the credential path.
	if !IsSecretField("client_secret") || IsSecretField("client_id") {
		t.Fatal("client_secret must remain a secret field and client_id must not become one")
	}
}
