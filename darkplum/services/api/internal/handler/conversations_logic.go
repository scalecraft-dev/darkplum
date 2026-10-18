package handler

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	roleUser      = "user"
	roleAssistant = "assistant"

	conversationTitleMaxLen       = 200
	conversationTitleTruncateLen  = 60
	conversationDefaultTitle      = "New conversation"
	conversationContentMaxBytes   = 16 << 20 // 16 MiB; mirrors migration 025's CHECK on conversation_messages.content, = chatBodyMaxBytes
	conversationImportKeyMaxLen   = 200      // mirrors migration 012's CHECK on conversations.import_key
	conversationRequestIDMaxLen   = 200      // defensive cap; request_id is an opaque hex string in practice (see cmd/main.go requestID())
	conversationsListDefaultLimit = 200
	conversationsListMaxLimit     = 200
)

// deriveConversationTitle picks a title for a newly created conversation when
// the caller didn't supply one explicitly: the first user message, truncated
// to conversationTitleTruncateLen runes, or conversationDefaultTitle if there
// is no user message at all (e.g. an empty initial POST /conversations).
func deriveConversationTitle(messages []messageInput) string {
	for _, m := range messages {
		if m.Role == roleUser {
			return truncateTitle(m.Content)
		}
	}
	return conversationDefaultTitle
}

func truncateTitle(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return conversationDefaultTitle
	}
	r := []rune(s)
	if len(r) > conversationTitleTruncateLen {
		return string(r[:conversationTitleTruncateLen])
	}
	return s
}

// validConversationTitle mirrors conversations.title's CHECK constraint
// (char_length BETWEEN 1 AND 200), so a bad explicit title is rejected with
// 400 before it ever reaches the DB CHECK as a 500.
func validConversationTitle(title string) bool {
	n := utf8.RuneCountInString(title)
	return n >= 1 && n <= conversationTitleMaxLen
}

// validImportKey mirrors conversations.import_key's CHECK constraint. A nil
// or empty import key is fine (no idempotency requested); non-empty is
// capped.
// conversationModelMaxLen bounds conversations.model, which is a model or
// alias id (the gateway caps alias names at 63 and no vendor id approaches
// 200). It is echoed in every list response, so it is bounded like title.
const conversationModelMaxLen = 200

func validConversationModel(model string) bool {
	return utf8.RuneCountInString(model) <= conversationModelMaxLen
}

func validImportKey(importKey *string) bool {
	if importKey == nil {
		return true
	}
	return utf8.RuneCountInString(*importKey) <= conversationImportKeyMaxLen
}

// validMessageInput enforces the same shape as conversation_messages' CHECK
// constraints (role, content size), plus a non-empty-content rule the schema
// doesn't need but which catches an accidental blank turn before it's
// persisted. It does not look at RequestID -- that's resolved separately
// against inference_events (see resolveAssistantProvenance in
// conversations.go), never trusted as-is.
func validMessageInput(m messageInput) error {
	if m.Role != roleUser && m.Role != roleAssistant {
		return fmt.Errorf("role must be 'user' or 'assistant'")
	}
	if m.Content == "" {
		return fmt.Errorf("content is required")
	}
	if len(m.Content) > conversationContentMaxBytes {
		return fmt.Errorf("content exceeds %d bytes", conversationContentMaxBytes)
	}
	if m.RequestID != nil && utf8.RuneCountInString(*m.RequestID) > conversationRequestIDMaxLen {
		return fmt.Errorf("request_id exceeds %d characters", conversationRequestIDMaxLen)
	}
	return nil
}

// validateInitialMessages validates the optional messages array on POST
// /conversations: any number of messages (including zero, e.g. the local
// history import case), each individually valid. Unlike append, there is no
// count or role-order restriction -- an imported historical conversation can
// be an arbitrary user/assistant sequence.
func validateInitialMessages(msgs []messageInput) error {
	for i, m := range msgs {
		if err := validMessageInput(m); err != nil {
			return fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	return nil
}

// validateAppendMessages validates the body of POST
// /conversations/:id/messages: 1 or 2 messages representing exactly a user
// turn, a user turn followed by an assistant turn, or an assistant turn alone
// (the reply to an already-stored user turn, e.g. after a regenerate has
// truncated the previous reply) -- never two of the same role.
func validateAppendMessages(msgs []messageInput) error {
	if len(msgs) < 1 || len(msgs) > 2 {
		return fmt.Errorf("messages must contain 1 or 2 items")
	}
	for i, m := range msgs {
		if err := validMessageInput(m); err != nil {
			return fmt.Errorf("messages[%d]: %w", i, err)
		}
	}
	if len(msgs) == 2 {
		if msgs[0].Role != roleUser {
			return fmt.Errorf("messages[0] must have role 'user'")
		}
		if msgs[1].Role != roleAssistant {
			return fmt.Errorf("messages[1] must have role 'assistant'")
		}
	}
	return nil
}
