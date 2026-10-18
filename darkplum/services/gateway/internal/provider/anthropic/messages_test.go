package anthropic

import (
	"errors"
	"strings"
	"testing"

	"github.com/scalecraft-dev/darkplum/gateway/internal/provider"
)

func msg(role, content string) provider.Message {
	return provider.Message{Role: role, Content: content}
}

func TestNormalizeMessages(t *testing.T) {
	cases := []struct {
		name       string
		in         []provider.Message
		wantSystem string
		wantTurns  []provider.Message
		wantErr    string // substring of the expected error message, "" if no error expected
	}{
		{
			name:       "single user message",
			in:         []provider.Message{msg("user", "hello")},
			wantSystem: "",
			wantTurns:  []provider.Message{msg("user", "hello")},
		},
		{
			name: "system hoisted out",
			in: []provider.Message{
				msg("system", "be nice"),
				msg("user", "hi"),
			},
			wantSystem: "be nice",
			wantTurns:  []provider.Message{msg("user", "hi")},
		},
		{
			name: "multiple system turns joined with blank line, order preserved",
			in: []provider.Message{
				msg("system", "first"),
				msg("user", "hi"),
				msg("system", "second"),
				msg("assistant", "hello"),
				msg("user", "how are you"),
			},
			wantSystem: "first\n\nsecond",
			wantTurns: []provider.Message{
				msg("user", "hi"),
				msg("assistant", "hello"),
				msg("user", "how are you"),
			},
		},
		{
			name: "consecutive same-role turns merged with blank line",
			in: []provider.Message{
				msg("user", "part one"),
				msg("user", "part two"),
				msg("assistant", "reply one"),
				msg("assistant", "reply two"),
				msg("user", "thanks"),
			},
			wantSystem: "",
			wantTurns: []provider.Message{
				msg("user", "part one\n\npart two"),
				msg("assistant", "reply one\n\nreply two"),
				msg("user", "thanks"),
			},
		},
		{
			name: "empty-content turns dropped",
			in: []provider.Message{
				msg("user", "hi"),
				msg("assistant", "   "),
				msg("user", "still there?"),
			},
			wantSystem: "",
			wantTurns: []provider.Message{
				// The empty assistant turn is dropped, so the two user turns
				// become adjacent and merge.
				msg("user", "hi\n\nstill there?"),
			},
		},
		{
			name:    "empty system-only content dropped, leaving no turns",
			in:      []provider.Message{msg("system", ""), msg("user", "   ")},
			wantErr: "at least one",
		},
		{
			name:    "no messages at all",
			in:      nil,
			wantErr: "at least one",
		},
		{
			name:    "leading assistant turn rejected",
			in:      []provider.Message{msg("assistant", "hi there"), msg("user", "hello")},
			wantErr: "must start with a user message",
		},
		{
			name:    "trailing assistant turn rejected (prefill)",
			in:      []provider.Message{msg("user", "finish this: "), msg("assistant", "The answer is (")},
			wantErr: "must not end with an assistant message",
		},
		{
			name:    "unsupported role rejected",
			in:      []provider.Message{msg("function", "some tool result")},
			wantErr: `role \"function\" is not supported`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeMessages(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got none (result: %+v)", tc.wantErr, got)
				}
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("expected an *APIError, got %T: %v", err, err)
				}
				if apiErr.HTTPStatus() != 400 {
					t.Errorf("expected HTTP 400, got %d", apiErr.HTTPStatus())
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("expected error to contain %q, got %q", tc.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.System != tc.wantSystem {
				t.Errorf("System = %q, want %q", got.System, tc.wantSystem)
			}
			if len(got.Turns) != len(tc.wantTurns) {
				t.Fatalf("Turns = %+v, want %+v", got.Turns, tc.wantTurns)
			}
			for i := range got.Turns {
				if got.Turns[i].Role != tc.wantTurns[i].Role || got.Turns[i].Content != tc.wantTurns[i].Content {
					t.Errorf("Turns[%d] = %+v, want %+v", i, got.Turns[i], tc.wantTurns[i])
				}
			}
		})
	}
}
