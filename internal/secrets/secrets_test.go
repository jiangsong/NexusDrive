package secrets

import "testing"

func TestScanFindsTheKnownShapesAndNothingElse(t *testing.T) {
	cases := map[string]string{
		"aws-access-key":      "key = AKIAIOSFODNN7EXAMPLE\n",
		"private-key":         "-----BEGIN RSA PRIVATE KEY-----\nMIIE...\n",
		"github-token":        "token ghp_abcdefghijklmnopqrstuvwxyz0123456789ABCD\n",
		"slack-token":         "xoxb-1234567890-abcdefghij\n",
		"gitlab-token":        "glpat-abcdefghij1234567890\n",
		"openai-key":          "sk-abcdefghijklmnopqrstuvwxyz\n",
		"password-assignment": "line one\npassword = hunter2hunter2\n",
	}
	for rule, text := range cases {
		got := Scan([]byte(text))
		if len(got) == 0 || got[0].Rule != rule {
			t.Errorf("%s: %+v", rule, got)
		}
	}
	if got := Scan([]byte("line one\npassword = hunter2hunter2\n")); len(got) != 1 || got[0].Line != 2 {
		t.Errorf("line number: %+v", got)
	}
	for _, clean := range []string{"", "# Quarterly report\n\nRevenue grew 12%.\n", "password: short\n", "the word secret alone\n", "sk-tooshort\n"} {
		if got := Scan([]byte(clean)); len(got) != 0 {
			t.Errorf("clean text %q flagged: %+v", clean, got)
		}
	}
	if got := Scan([]byte("AKIAIOSFODNN7EXAMPLE\x00binary")); len(got) != 0 {
		t.Errorf("binary data scanned: %+v", got)
	}
	if len(Rules()) != len(rules) {
		t.Fatal("Rules does not list every rule")
	}
}
