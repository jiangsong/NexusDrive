package trigger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"cloudfs/internal/agent"
	"cloudfs/internal/config"
)

// outputCap is how much of each stream a delivery row keeps.
const outputCap = 64 << 10

// outputTruncatedMarker ends a stream that hit outputCap; the delivery view
// derives its truncated flag from it.
const outputTruncatedMarker = agent.OutputTruncatedMarker

// stderrSeparator joins the two streams into the row's single output field.
const stderrSeparator = "\n--- stderr ---\n"

// errTimeout is wrapped into the error of a command that ran past its
// timeout and was killed.
var errTimeout = errors.New("trigger: command timed out")

// childEnvKeys are the daemon variables a child inherits; everything else
// (tokens, proxy settings, whatever the user's shell exported) stays out.
var childEnvKeys = []string{"PATH", "HOME", "LANG"}

// runExec runs a without a shell (docs/agent-roadmap.md §5.4). A vars key
// ("{path}", "{kind}", "{uri}", "{prompt}") replaces an argv element only
// when the element is exactly that key; a placeholder inside a longer
// string is passed as written. The child gets a scrubbed environment plus
// CLOUDFS_PATH, CLOUDFS_KIND and CLOUDFS_URI, its own process group, and
// is killed as a group when a.Timeout or ctx expires. Each stream is cut
// at outputCap. The streams are returned even when err is set, so a
// failed run still leaves something to read in the delivery row.
func runExec(ctx context.Context, a config.ExecAction, vars map[string]string) (stdout, stderr []byte, err error) {
	if len(a.Command) == 0 || a.Command[0] == "" {
		return nil, nil, errors.New("trigger: exec has no command")
	}
	argv := make([]string, len(a.Command))
	argv[0] = a.Command[0]
	for i, arg := range a.Command[1:] {
		if v, ok := vars[arg]; ok {
			argv[i+1] = v
		} else {
			argv[i+1] = arg
		}
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = config.DefaultExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = a.Cwd
	cmd.Env = childEnv(vars)
	out := &limitedBuffer{max: outputCap}
	errOut := &limitedBuffer{max: outputCap}
	cmd.Stdout, cmd.Stderr = out, errOut
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	// A grandchild that inherited the pipes and survived the kill (it
	// cannot on a platform where killGroup works, but WaitDelay is the
	// guarantee, not the hope) is abandoned after this long.
	cmd.WaitDelay = 5 * time.Second

	err = cmd.Run()
	switch {
	case err == nil:
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		err = fmt.Errorf("%w after %s", errTimeout, timeout)
	case ctx.Err() != nil:
		err = fmt.Errorf("trigger: command interrupted: %w", ctx.Err())
	default:
		err = fmt.Errorf("trigger: %w", err)
	}
	return out.Bytes(), errOut.Bytes(), err
}

// childEnv builds the child's environment: the daemon's PATH, HOME and
// LANG, and the three CLOUDFS_* variables (always set, empty for a rescan
// delivery that has no path).
func childEnv(vars map[string]string) []string {
	env := make([]string, 0, len(childEnvKeys)+3)
	for _, k := range childEnvKeys {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	env = append(env,
		"CLOUDFS_PATH="+vars["{path}"],
		"CLOUDFS_KIND="+vars["{kind}"],
		"CLOUDFS_URI="+vars["{uri}"],
	)
	return env
}

// joinOutput is the delivery row's output: stdout, then stderr after a
// separator when there is any.
func joinOutput(stdout, stderr []byte) string {
	if len(stderr) == 0 {
		return string(stdout)
	}
	return string(stdout) + stderrSeparator + string(stderr)
}

// limitedBuffer keeps the first max bytes written to it and marks the cut.
// Writes past the cap succeed, so the child never sees a broken pipe.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.truncated {
		return n, nil
	}
	room := b.max - b.buf.Len()
	if len(p) > room {
		b.buf.Write(p[:room])
		b.buf.WriteString(outputTruncatedMarker)
		b.truncated = true
		return n, nil
	}
	b.buf.Write(p)
	return n, nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }
