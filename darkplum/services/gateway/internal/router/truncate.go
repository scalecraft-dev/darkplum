package router

// DefaultTruncateChars is the head+tail truncation budget applied to the
// last user turn before it is handed to a Classifier (2000 chars per the
// routed-alias contract).
const DefaultTruncateChars = 2000

// Truncate returns s unchanged if it is at most max runes long. Otherwise it
// returns the first max/2 runes concatenated with the last max-(max/2) runes
// (dropping the middle), keeping the total rune count at exactly max.
//
// This "head+tail" shape is deliberate: for a long turn, both the opening
// context (often instructions or a pasted document's start) and the closing
// context (often the actual question, appended last) carry more routing
// signal than whatever falls in the middle.
//
// Operates on runes, not bytes, so truncation never splits a multi-byte
// UTF-8 sequence.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	head := max / 2
	tail := max - head
	return string(runes[:head]) + string(runes[len(runes)-tail:])
}
