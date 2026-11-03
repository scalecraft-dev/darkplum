package commands

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/scalecraft-dev/darkplum/cli/internal/install"
)

// deferredLog is a console writer that also keeps the run in a log file under
// the install's workspace. init does not know which workspace it is writing to
// until the interview is over, so what was printed before that is held in
// memory and flushed when the log opens. Nothing is held for a run that never
// opens one (a dry run or a refusal writes nothing).
//
// Only what the command prints goes in. Passwords are read without echo and
// never printed, so they never reach the log; keep it that way: never print a
// secret from a command that logs.
type deferredLog struct {
	mu      sync.Mutex
	console io.Writer
	buf     bytes.Buffer
	f       *os.File
	path    string
}

func newDeferredLog(console io.Writer) *deferredLog { return &deferredLog{console: console} }

// ansiRe matches the colour and style escapes the console output carries. The
// log is read in an editor or pasted into a ticket, where they are noise.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func (d *deferredLog) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	plain := ansiRe.ReplaceAll(p, nil)
	if d.f != nil {
		_, _ = d.f.Write(plain)
	} else {
		d.buf.Write(plain)
	}
	return d.console.Write(p)
}

// logError records, in the log only, an error the command is about to return
// that nothing printed (a reported error, or an abort, is already on screen).
func (d *deferredLog) logError(err error) {
	if err == nil || IsReported(err) || errors.Is(err, errAborted) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	line := "error: " + err.Error() + "\n"
	if d.f != nil {
		_, _ = d.f.WriteString(line)
	} else {
		d.buf.WriteString(line)
	}
}

// attach opens <workspace>/logs/<command>-<time>.log and flushes what was
// printed so far into it. A failure to open it is reported on the console and
// the run carries on: a log must never block the customer.
func (d *deferredLog) attach(in install.Install, command string, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f != nil {
		return
	}
	f, path, err := openCommandLog(in, command, now)
	if err != nil {
		fmt.Fprintf(d.console, "  ! could not write a log under %s: %v\n", in.LogsDir(), err)
		d.buf.Reset()
		return
	}
	_, _ = f.Write(d.buf.Bytes())
	d.buf.Reset()
	d.f, d.path = f, path
}

func (d *deferredLog) Path() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.path
}

func (d *deferredLog) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

// newRunLog is a console writer that also logs to <workspace>/logs/<command>-<time>.log.
func newRunLog(in install.Install, command string, console io.Writer, now time.Time) *deferredLog {
	d := newDeferredLog(console)
	d.attach(in, command, now)
	return d
}
