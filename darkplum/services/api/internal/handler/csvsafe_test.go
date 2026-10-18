package handler

import "testing"

// TestCSVSafeText pins the formula neutralisation for user-controlled cells:
// a leading formula trigger gets the apostrophe prefix; plain text and empty
// cells are untouched.
func TestCSVSafeText(t *testing.T) {
	cases := map[string]string{
		`=HYPERLINK("http://evil","x")`: `'=HYPERLINK("http://evil","x")`,
		"+1-800":                        "'+1-800",
		"-5":                            "'-5",
		"@SUM(1)":                       "'@SUM(1)",
		"|cmd":                          "'|cmd",
		"":                              "",
		"plain name":                    "plain name",
		"claude-opus-5-5":               "claude-opus-5-5",
	}
	for in, want := range cases {
		if got := csvSafeText(in); got != want {
			t.Errorf("csvSafeText(%q) = %q, want %q", in, got, want)
		}
	}
}
