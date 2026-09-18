package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"cloudfs/internal/config"
	"cloudfs/internal/control"
)

// controlProbeTimeout bounds the "is a daemon already running" question. It
// is short on purpose: the answer gates start-up, and an unresponsive socket
// is indistinguishable from a dead one for this decision.
const controlProbeTimeout = 2 * time.Second

// entry is how this process was started. It exists for one decision: whether
// finishing the start-up is supposed to put a browser in front of someone.
type entry int

const (
	// entryMount is `cloudfs mount`, the command a launchd plist or a
	// systemd unit runs.
	entryMount entry = iota
	// entryStart is the no-argument front door, run by a person.
	entryStart
)

// opensBrowser reports whether an entry point opens the dashboard once the
// control plane answers.
//
// `cloudfs mount` must not: it is the service path, and a service that pops a
// browser window on boot — or on every automatic restart — is a defect that
// the person cannot even attribute to us.
func opensBrowser(e entry) bool { return e != entryMount }

// startAction is what the no-argument entry point does about the
// configuration it found.
type startAction int

const (
	// startSetup serves the configuration screen: there is either no
	// configuration or nothing in it that can be mounted yet.
	startSetup startAction = iota
	// startMount mounts what is configured and opens the dashboard.
	startMount
)

func (a startAction) String() string {
	if a == startMount {
		return "mount"
	}
	return "setup"
}

// startActionFor decides between setting up and mounting. It reuses the
// wizard's own staging so that the front door and `cloudfs setup` can never
// disagree about whether a configuration is usable.
func startActionFor(path string) startAction {
	if stageFor(path) == setupAlreadyUsable {
		return startMount
	}
	return startSetup
}

// migrateLegacyLayout runs the one-time move for a command that is about to
// start a daemon, reporting it to out.
//
// Every entry point that starts a daemon needs this, not just the front door.
// A service unit running plain `cloudfs mount` resolves the default path,
// which is now in the new root; without the migration it would find no
// configuration at all and fail, with the person's real configuration sitting
// untouched in the old location. Read-only commands deliberately do not call
// it: moving a person's files is not something `cloudfs status` should do.
func migrateLegacyLayout(path string, out io.Writer) error {
	m, err := config.MigrateLegacyLayout(path, daemonAnswering)
	if err != nil {
		return err
	}
	if m.Ran {
		fmt.Fprintf(out, "moved your CloudFS files into %s\n", m.To)
		fmt.Fprintf(out, "  the block cache stayed in %s and is still in use\n", m.BlocksKept)
		fmt.Fprintf(out, "  the previous configuration is kept at %s.moved-to-cloudfs\n", m.From)
	}
	return nil
}

// startConfigPath resolves the configuration the front door should use, and
// migrates a pre-single-root installation into ~/.cloudfs on the way.
//
// The migration is attached to this resolution rather than to the daemon
// because it has to happen before anything opens the journal: the point of
// moving the files is that the old location is a directory the system treats
// as disposable, and a daemon that has already opened them there would be
// writing into an unlinked directory after the move.
func startConfigPath(f *flags, out io.Writer) (string, error) {
	path := f.str("config", defaultConfigPath())
	if err := migrateLegacyLayout(path, out); err != nil {
		return "", err
	}
	return path, nil
}

// daemonAnswering reports whether a daemon holds the control endpoints named
// by a configuration. A migration that renamed the journal out from under a
// running daemon would leave it writing into an unlinked directory.
func daemonAnswering(socket string) bool {
	if socket == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlProbeTimeout)
	defer cancel()
	_, online, err := control.FetchStatus(ctx, socket, "")
	return err == nil && online
}

// cmdStart is `cloudfs` with no arguments: the product's front door.
//
// It exists because the two things a person can want at that moment are not
// the same thing, and asking them to know which is which is the onboarding
// problem this whole flow is about. With nothing configured it serves the
// setup screen; with a usable configuration it mounts and shows the dashboard.
func cmdStart(ctx context.Context, args []string) error {
	f := parseFlags(args, "force", "no-open")
	path, err := startConfigPath(f, os.Stdout)
	if err != nil {
		return err
	}
	if startActionFor(path) == startMount {
		return runMount(ctx, mountArgs(f, path), entryStart)
	}
	return cmdSetup(ctx, setupArgs(f, path))
}

// mountArgs and setupArgs rebuild the command line for the handler the front
// door chose, rather than forwarding what it was given.
//
// Forwarding is not safe here: parseFlags treats an unrecognized "--name" as
// taking the next token as its value, so handing `--force` to the mount path —
// which has no such flag — would silently swallow the mount point after it.
// Each handler is given the flags it actually declares.
func mountArgs(f *flags, path string) []string {
	args := []string{"--config", path}
	if f.bool("no-open") {
		args = append(args, "--no-open")
	}
	return append(args, f.args...)
}

func setupArgs(f *flags, path string) []string {
	args := []string{"--config", path}
	if f.bool("force") {
		args = append(args, "--force")
	}
	if f.bool("no-open") {
		args = append(args, "--no-open")
	}
	if listen := f.str("listen", ""); listen != "" {
		args = append(args, "--listen", listen)
	}
	return args
}
