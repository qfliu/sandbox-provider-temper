package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// MaxOutput caps how much of stdout and of stderr an exec returns, so a
// runaway command can't exhaust the API's memory.
const MaxOutput = 1 << 20 // 1 MiB each

// ExecResult is what a command produced.
type ExecResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"` // stdout or stderr hit MaxOutput
	DurationMS int64  `json:"duration_ms"`
}

// timeoutExitCode is what coreutils `timeout` exits with when it kills.
const timeoutExitCode = 124

// Run executes command in workdir (relative to the home directory if not
// absolute) with a time limit. The command runs under the sandbox's own
// `timeout`, so it's killed inside the sandbox even if this side goes away;
// a client-side deadline a little later is the backstop.
func Run(ctx context.Context, client *ssh.Client, command, workdir string, limit time.Duration) (ExecResult, error) {
	sess, err := client.NewSession()
	if err != nil {
		return ExecResult{}, fmt.Errorf("open session: %w", err)
	}
	defer sess.Close()

	stdout := &cappedBuffer{max: MaxOutput}
	stderr := &cappedBuffer{max: MaxOutput}
	sess.Stdout, sess.Stderr = stdout, stderr

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- sess.Run(wrapCommand(command, workdir, limit)) }()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(limit + 10*time.Second):
		sess.Close() // the in-sandbox timeout should have fired long ago
		runErr = <-done
	case <-ctx.Done():
		sess.Close()
		<-done
		return ExecResult{}, ctx.Err()
	}

	res := ExecResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}
	var exitErr *ssh.ExitError
	var missing *ssh.ExitMissingError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitStatus()
	case errors.As(runErr, &missing):
		// No exit status: killed by our deadline, or sshd died mid-command.
		// Only call it a timeout if the time limit really passed.
		res.ExitCode, res.TimedOut = -1, time.Since(start) >= limit
		return res, nil
	default:
		return ExecResult{}, fmt.Errorf("run command: %w", runErr)
	}
	res.TimedOut = res.ExitCode == timeoutExitCode && time.Since(start) >= limit
	return res, nil
}

// wrapCommand builds the remote command line: cd into workdir, then run the
// user's command in bash under `timeout` (TERM, then KILL 5s later).
func wrapCommand(command, workdir string, limit time.Duration) string {
	secs := int(limit.Seconds())
	if secs < 1 {
		secs = 1
	}
	cd := "cd ~"
	if workdir != "" {
		cd = "cd " + shellQuote(workdir)
	}
	return fmt.Sprintf("%s && exec timeout -k 5 %d bash -c %s", cd, secs, shellQuote(command))
}

// shellQuote wraps s in single quotes for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cappedBuffer keeps the first max bytes written and drops the rest,
// remembering that it did.
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil // pretend we took it all so the command isn't blocked
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string { return c.buf.String() }
