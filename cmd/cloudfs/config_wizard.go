package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"cloudfs/internal/provider"

	"golang.org/x/term"
)

// `config add` takes every public field as a flag, which is what a script
// wants and what a person setting up their first remote does not: they have to
// know the field names before they can ask what the field names are.
//
// So when a field is missing and there is a terminal, it is asked for. The
// questions come from the drivers themselves (provider.RegisterFields), not
// from a table here keyed by provider name: that table would go stale the first
// time a driver changed, and a new driver would be invisible to it.
//
// Nothing about the non-interactive path changes. Without a terminal, or with
// every required field supplied, no question is asked and the command behaves
// exactly as before — a script must not start blocking on a prompt.

// interactiveInput reports whether questions can be asked, and returns the
// reader to ask them through.
func interactiveInput(c configIO) (*bufio.Reader, bool) {
	f, ok := c.In.(*os.File)
	if ok && !term.IsTerminal(int(f.Fd())) {
		return nil, false
	}
	if !ok && c.ReadSecret == nil {
		// A test that supplies a plain reader but no secret hook is scripted
		// input, not a person; treat it as non-interactive.
		return nil, false
	}
	if c.In == nil {
		return nil, false
	}
	return bufio.NewReader(c.In), true
}

// ask puts one question and returns the trimmed answer, or def when empty.
func ask(in *bufio.Reader, out io.Writer, prompt, def, example string) (string, error) {
	suffix := ""
	switch {
	case def != "":
		suffix = fmt.Sprintf(" [%s]", def)
	case example != "":
		suffix = fmt.Sprintf(" (e.g. %s)", example)
	}
	fmt.Fprintf(out, "%s%s: ", prompt, suffix)
	line, err := in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", err
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

// chooseType asks which backend to add. It lists the types that describe
// themselves; a type without a description can still be given with --type.
func chooseType(in *bufio.Reader, out io.Writer) (string, error) {
	types := provider.DescribedTypes()
	if len(types) == 0 {
		return "", errors.New("config add: no provider types are registered")
	}
	fmt.Fprintf(out, "Backend types: %s\n", strings.Join(types, ", "))
	for attempt := 0; attempt < 3; attempt++ {
		answer, err := ask(in, out, "Type", "", types[0])
		if err != nil {
			return "", err
		}
		for _, t := range provider.Types() {
			if answer == t {
				return answer, nil
			}
		}
		fmt.Fprintf(out, "%q is not a registered type.\n", answer)
	}
	return "", errors.New("config add: no valid type given")
}

// collectFields asks for the public fields the driver declared that the caller
// did not already supply. A required field with no answer is an error rather
// than a silent gap: the remote would fail to build later, further from the
// place that could have said what was missing.
func collectFields(in *bufio.Reader, out io.Writer, typ string, have map[string]string) error {
	for _, field := range provider.Fields(typ) {
		if _, ok := have[field.Name]; ok {
			continue
		}
		answer, err := ask(in, out, field.Prompt, field.Default, field.Example)
		if err != nil {
			return err
		}
		if answer == "" {
			if field.Required {
				return fmt.Errorf("config add: %s is required", field.Name)
			}
			continue
		}
		have[field.Name] = answer
	}
	return nil
}

// missingRequired names the declared required fields a caller did not supply.
// It is what the non-interactive path reports instead of leaving the failure to
// the driver's own constructor much later.
func missingRequired(typ string, have map[string]string) []string {
	var missing []string
	for _, field := range provider.Fields(typ) {
		if !field.Required {
			continue
		}
		if value, ok := have[field.Name]; !ok || value == "" {
			missing = append(missing, field.Name)
		}
	}
	return missing
}

// credentialHint tells the user what `config auth` will want, using what the
// driver declared rather than a lookup table over provider names.
func credentialHint(typ string) string {
	creds := provider.CredentialsFor(typ)
	if creds.Note != "" {
		return creds.Note
	}
	if len(creds.Fields) > 0 {
		return strings.Join(creds.Fields, " or ")
	}
	return ""
}
