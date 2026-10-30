package commands

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// errAborted means the operator ended the session (EOF / Ctrl-D) before init
// finished. Nothing is written when it is returned.
var errAborted = reportedError{"aborted — nothing was written"}

// reportedError is a failure init has already shown as a ✗ row with a hint, so
// the CLI exits non-zero without printing a second, bare `darkplum: error:`.
type reportedError struct{ msg string }

func (e reportedError) Error() string { return e.msg }

// IsReported tells main not to print err again.
func IsReported(err error) bool {
	var r reportedError
	return errors.As(err, &r)
}

// Option is one numbered choice in a Select.
type Option struct {
	Label  string
	Detail string // dimmed after the label
}

// Prompter is the interactive surface of init. The terminal implementation is
// below; tests drive init through the same interface with scripted answers, so
// no TTY is needed.
type Prompter interface {
	// Ask reads a line. An empty answer takes def. A validation error is
	// printed with a hint and the question is asked again.
	Ask(label, def string, validate func(string) error) (string, error)
	// Confirm asks a yes/no question. An empty answer takes def.
	Confirm(label string, def bool) (bool, error)
	// Select shows a numbered list and returns the chosen index. An empty
	// answer takes def (0-based).
	Select(label string, opts []Option, def int) (int, error)
	// AskSecret reads a line without echoing it. It refuses when there is no
	// terminal to read from: a secret never comes from a pipe.
	AskSecret(label string) (string, error)
	// MultiSelect shows a numbered list and returns the chosen indexes in
	// order: "1,3", "1-3", "none"; an empty answer takes defs (0-based).
	MultiSelect(label string, opts []Option, defs []int) ([]int, error)
}

// errNoTerminal is AskSecret's refusal when stdin is not a terminal.
var errNoTerminal = errors.New("a password can only be typed at a terminal (stdin is not one)")

// ioPrompter reads answers from r and writes prompts to w: `? Label [default]`,
// numbered choices, no arrow keys, so it works in a pipe and over ssh.
type ioPrompter struct {
	r *bufio.Reader
	w io.Writer
	// secret reads one line without echo; nil when there is no terminal.
	// Tests script it.
	secret func() (string, error)
}

func newIOPrompter(r io.Reader, w io.Writer) *ioPrompter {
	return &ioPrompter{r: bufio.NewReader(r), w: w}
}

// newTermPrompter is the prompter on the process's own stdin/stdout. Secrets
// are read from the terminal with echo off, and refused when stdin is a pipe.
func newTermPrompter() *ioPrompter {
	p := newIOPrompter(os.Stdin, os.Stdout)
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		p.secret = func() (string, error) {
			b, err := term.ReadPassword(fd)
			if err != nil {
				return "", errAborted
			}
			return string(b), nil
		}
	}
	return p
}

func (p *ioPrompter) line() (string, error) {
	s, err := p.r.ReadString('\n')
	if err != nil && (err != io.EOF || s == "") {
		return "", errAborted
	}
	return strings.TrimSpace(s), nil
}

func (p *ioPrompter) Ask(label, def string, validate func(string) error) (string, error) {
	for {
		if def != "" {
			fmt.Fprintf(p.w, "%s %s %s ", sInfo("?"), label, sDim("["+def+"]"))
		} else {
			fmt.Fprintf(p.w, "%s %s ", sInfo("?"), label)
		}
		ans, err := p.line()
		if err != nil {
			fmt.Fprintln(p.w)
			return "", err
		}
		if ans == "" {
			ans = def
		}
		if validate != nil {
			if verr := validate(ans); verr != nil {
				fmt.Fprintf(p.w, "  %s %s\n", sFail("✗"), verr.Error())
				continue
			}
		}
		return ans, nil
	}
}

func (p *ioPrompter) Confirm(label string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		fmt.Fprintf(p.w, "%s %s %s ", sInfo("?"), label, sDim("["+hint+"]"))
		ans, err := p.line()
		if err != nil {
			fmt.Fprintln(p.w)
			return false, err
		}
		switch strings.ToLower(ans) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintf(p.w, "  %s answer y or n\n", sFail("✗"))
	}
}

// options prints the numbered choices under a question.
func (p *ioPrompter) options(label string, opts []Option) {
	fmt.Fprintf(p.w, "%s %s\n", sInfo("?"), label)
	width := 0
	for _, o := range opts {
		if len(o.Label) > width {
			width = len(o.Label)
		}
	}
	for i, o := range opts {
		line := fmt.Sprintf("    %d) %-*s", i+1, width, o.Label)
		if o.Detail != "" {
			line += "  " + sDim(o.Detail)
		} else {
			line = strings.TrimRight(line, " ")
		}
		fmt.Fprintln(p.w, line)
	}
}

func (p *ioPrompter) Select(label string, opts []Option, def int) (int, error) {
	p.options(label, opts)
	for {
		fmt.Fprintf(p.w, "  choose %s ", sDim(fmt.Sprintf("[%d]", def+1)))
		ans, err := p.line()
		if err != nil {
			fmt.Fprintln(p.w)
			return 0, err
		}
		if ans == "" {
			return def, nil
		}
		n, perr := strconv.Atoi(ans)
		if perr == nil && n >= 1 && n <= len(opts) {
			return n - 1, nil
		}
		fmt.Fprintf(p.w, "  %s enter a number from 1 to %d\n", sFail("✗"), len(opts))
	}
}

func (p *ioPrompter) AskSecret(label string) (string, error) {
	if p.secret == nil {
		return "", errNoTerminal
	}
	fmt.Fprintf(p.w, "%s %s ", sInfo("?"), label)
	v, err := p.secret()
	fmt.Fprintln(p.w) // the terminal swallowed the Enter
	return v, err
}

func (p *ioPrompter) MultiSelect(label string, opts []Option, defs []int) ([]int, error) {
	p.options(label, opts)
	def := "none"
	if len(defs) > 0 {
		parts := make([]string, len(defs))
		for i, d := range defs {
			parts[i] = strconv.Itoa(d + 1)
		}
		def = strings.Join(parts, ",")
	}
	for {
		fmt.Fprintf(p.w, "  choose %s %s ", sDim("["+def+"]"), sDim("(e.g. 1,3 or 1-3; none)"))
		ans, err := p.line()
		if err != nil {
			fmt.Fprintln(p.w)
			return nil, err
		}
		if ans == "" {
			return append([]int(nil), defs...), nil
		}
		got, perr := parseChoices(ans, len(opts))
		if perr == nil {
			return got, nil
		}
		fmt.Fprintf(p.w, "  %s %s\n", sFail("✗"), perr.Error())
	}
}

// parseChoices reads "1,3", "1-3", "2, 4-5" or "none" into sorted, distinct
// 0-based indexes below n.
func parseChoices(s string, n int) ([]int, error) {
	bad := fmt.Errorf("enter numbers from 1 to %d, like 1,3 or 1-3, or none", n)
	if strings.EqualFold(strings.TrimSpace(s), "none") {
		return []int{}, nil
	}
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, bad
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, bad
			}
		}
		if a < 1 || b > n || a > b {
			return nil, bad
		}
		for i := a; i <= b; i++ {
			seen[i-1] = true
		}
	}
	out := make([]int, 0, len(seen))
	for i := 0; i < n; i++ {
		if seen[i] {
			out = append(out, i)
		}
	}
	return out, nil
}
