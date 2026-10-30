package commands

import "fmt"

// Brand palette — the single source of truth for CLI colors. Mapped directly
// from the brand palette hex values (the same ones services/web uses in
// src/tokens/tokens.css) to truecolor ANSI foreground
// escapes. Referenced by style.go (semantic helpers) and banner.go; nothing
// else should hardcode an ANSI color.
var (
	ansiReset = "\x1b[0m"
	ansiBold  = "\x1b[1m"

	ansiGrape = fg(0x6B, 0x2F, 0xA8) // #6B2FA8 — primary, for light surfaces
	ansiLilac = fg(0xA8, 0x77, 0xD9) // #A877D9 — dark-mode accent
	ansiMoss  = fg(0x5C, 0x70, 0x48) // #5C7048 — accent, used sparingly
	ansiSage  = fg(0xA8, 0xB8, 0x9A) // #A8B89A — supporting neutral / secondary text
	ansiAmber = fg(0xD9, 0xA4, 0x41) // warm amber — warnings: needs attention, not a failure
	ansiRed   = fg(0xC0, 0x50, 0x4D) // warm muted red — failures only (outside the garden palette)

	// ansiBrand is the brand color as it appears in the CLI. A terminal is a dark
	// surface, and the brand guide specifies Lilac as the dark-mode accent (Grape
	// reads too deep/subdued on black), so the wordmark and headers use Lilac.
	ansiBrand = ansiLilac
)

// fg returns the truecolor ANSI foreground escape for an RGB triplet.
func fg(r, g, b int) string {
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
}
