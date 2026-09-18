package handler

import (
	"strings"
	"testing"
)

func TestDeriveConversationTitle(t *testing.T) {
	longMsg := strings.Repeat("a", 100)

	cases := []struct {
		name     string
		messages []messageInput
		want     string
	}{
		{"no messages", nil, conversationDefaultTitle},
		{"only assistant message", []messageInput{{Role: roleAssistant, Content: "hi there"}}, conversationDefaultTitle},
		{"short user message", []messageInput{{Role: roleUser, Content: "hello world"}}, "hello world"},
		{"long user message truncated to 60 runes", []messageInput{{Role: roleUser, Content: longMsg}}, longMsg[:60]},
		{"first user message wins over later ones", []messageInput{
			{Role: roleAssistant, Content: "ignored"},
			{Role: roleUser, Content: "first"},
			{Role: roleUser, Content: "second"},
		}, "first"},
		{"blank user message falls back to default", []messageInput{{Role: roleUser, Content: "   "}}, conversationDefaultTitle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveConversationTitle(tc.messages); got != tc.want {
				t.Errorf("deriveConversationTitle(%+v) = %q, want %q", tc.messages, got, tc.want)
			}
		})
	}
}

func TestValidConversationTitle(t *testing.T) {
	cases := map[string]bool{
		"":                       false,
		"a":                      true,
		strings.Repeat("a", 200): true,
		strings.Repeat("a", 201): false,
		strings.Repeat("あ", 200): true, // multi-byte runes counted, not bytes
	}
	for title, want := range cases {
		if got := validConversationTitle(title); got != want {
			t.Errorf("validConversationTitle(len=%d) = %v, want %v", len([]rune(title)), got, want)
		}
	}
}

func TestValidImportKey(t *testing.T) {
	tooLong := strings.Repeat("k", 201)
	ok := strings.Repeat("k", 200)
	if !validImportKey(nil) {
		t.Error("nil import key should be valid")
	}
	if !validImportKey(&ok) {
		t.Error("200-char import key should be valid")
	}
	if validImportKey(&tooLong) {
		t.Error("201-char import key should be invalid")
	}
}

func TestValidMessageInput(t *testing.T) {
	tooBig := strings.Repeat("x", conversationContentMaxBytes+1)
	justRight := strings.Repeat("x", conversationContentMaxBytes)
	reqID := "abc123"
	tooLongReqID := strings.Repeat("r", conversationRequestIDMaxLen+1)

	cases := []struct {
		name    string
		msg     messageInput
		wantErr bool
	}{
		{"valid user", messageInput{Role: roleUser, Content: "hi"}, false},
		{"valid assistant with request_id", messageInput{Role: roleAssistant, Content: "hi", RequestID: &reqID}, false},
		{"invalid role", messageInput{Role: "system", Content: "hi"}, true},
		{"empty content", messageInput{Role: roleUser, Content: ""}, true},
		{"content at exactly the limit", messageInput{Role: roleUser, Content: justRight}, false},
		{"content over the limit", messageInput{Role: roleUser, Content: tooBig}, true},
		{"request_id too long", messageInput{Role: roleAssistant, Content: "hi", RequestID: &tooLongReqID}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validMessageInput(tc.msg)
			if (err != nil) != tc.wantErr {
				t.Errorf("validMessageInput(%+v) error = %v, wantErr %v", tc.msg, err, tc.wantErr)
			}
		})
	}
}

func TestValidateInitialMessages(t *testing.T) {
	if err := validateInitialMessages(nil); err != nil {
		t.Errorf("empty messages should be valid for creation, got %v", err)
	}
	// Any count, any order, including consecutive same-role turns (a bulk
	// import of historical local conversations may not alternate cleanly).
	many := []messageInput{
		{Role: roleUser, Content: "a"},
		{Role: roleUser, Content: "b"},
		{Role: roleAssistant, Content: "c"},
	}
	if err := validateInitialMessages(many); err != nil {
		t.Errorf("expected no error for a valid multi-message import, got %v", err)
	}

	bad := []messageInput{{Role: roleUser, Content: "ok"}, {Role: "bogus", Content: "x"}}
	if err := validateInitialMessages(bad); err == nil {
		t.Error("expected error for invalid role in messages[1]")
	} else if !strings.Contains(err.Error(), "messages[1]") {
		t.Errorf("expected error to reference messages[1], got %v", err)
	}
}

func TestValidateAppendMessages(t *testing.T) {
	cases := []struct {
		name    string
		msgs    []messageInput
		wantErr bool
	}{
		{"empty", nil, true},
		{"single user turn", []messageInput{{Role: roleUser, Content: "hi"}}, false},
		{"user then assistant", []messageInput{
			{Role: roleUser, Content: "hi"},
			{Role: roleAssistant, Content: "hello"},
		}, false},
		{"assistant alone is accepted (regenerated reply)", []messageInput{{Role: roleAssistant, Content: "hi"}}, false},
		{"assistant then user is rejected", []messageInput{{Role: roleAssistant, Content: "hi"}, {Role: roleUser, Content: "q"}}, true},
		{"user then user is rejected", []messageInput{
			{Role: roleUser, Content: "hi"},
			{Role: roleUser, Content: "hi again"},
		}, true},
		{"three messages rejected", []messageInput{
			{Role: roleUser, Content: "a"},
			{Role: roleAssistant, Content: "b"},
			{Role: roleUser, Content: "c"},
		}, true},
		{"invalid role in second slot", []messageInput{
			{Role: roleUser, Content: "a"},
			{Role: "system", Content: "b"},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAppendMessages(tc.msgs)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateAppendMessages(%+v) error = %v, wantErr %v", tc.msgs, err, tc.wantErr)
			}
		})
	}
}
