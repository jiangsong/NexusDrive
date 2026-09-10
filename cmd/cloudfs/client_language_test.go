package main

import (
	"testing"

	"cloudfs/internal/control"
)

// Every subcommand that reaches the daemon prints what the daemon rendered.
// Resolving the shell's language for local output and then not telling the
// daemon about it is how one session printed English for `status` and Chinese
// for `copies`. The language is settled once, where cliLang is.
func TestTheCommandTellsTheDaemonWhichLanguageToAnswerIn(t *testing.T) {
	if got := control.ClientLanguage(); got != cliLang {
		t.Fatalf("the daemon is asked for %q while this command prints %q", got, cliLang)
	}
}
