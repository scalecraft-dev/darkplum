package commands

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

var stdinReader = bufio.NewReader(os.Stdin)

func promptLine(label string) string {
	fmt.Printf("%s: ", label)
	line, _ := stdinReader.ReadString('\n')
	return strings.TrimSpace(line)
}

func promptIfEmpty(v, label string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return promptLine(label)
}

// readPassword reads a secret from stdin (no echo) or, with fromStdin, the
// whole piped stdin. The value never touches argv.
func readPassword(fromStdin bool, label string) (string, error) {
	if fromStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}
	fmt.Printf("%s: ", label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// yamlSingleQuote renders s as a YAML single-quoted scalar (only ' needs
// escaping, by doubling), safe for arbitrary passwords / secrets.
func yamlSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
