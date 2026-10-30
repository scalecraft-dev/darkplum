package commands

import "os"

// colorEnabled is resolved once against stdout (TTY + NO_COLOR), matching the
// banner's gating, so styled output degrades to plain text in pipes and CI.
var colorEnabled = useColor(os.Stdout)

func paint(codes, s string) string {
	if !colorEnabled {
		return s
	}
	return codes + s + ansiReset
}

// Semantic styles drawn from the brand palette (brand purple for structure; moss
// for success, used sparingly).
func sHeader(s string) string  { return paint(ansiBrand+ansiBold, s) } // section titles
func sCmd(s string) string     { return paint(ansiBrand, s) }          // command / flag names
func sSuccess(s string) string { return paint(ansiMoss, s) }           // ✓ outcomes
func sInfo(s string) string    { return paint(ansiLilac, s) }          // in-progress steps
func sDim(s string) string     { return paint(ansiSage, s) }           // secondary text
func sFail(s string) string    { return paint(ansiRed, s) }            // failures / not-ready
func sWarn(s string) string    { return paint(ansiAmber, s) }          // ! warnings
