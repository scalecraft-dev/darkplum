package openai

import (
	"encoding/json"
	"strings"
)

// droppedParams tracks which caller-set sampling parameters have already
// been dropped from the request after a 400 named one of them unsupported
// for the requested model — the adaptive rule this package uses instead of a
// per-model name list. The zero value means nothing has been dropped yet:
// the first attempt always sends every parameter the caller set.
type droppedParams struct {
	temperature bool
	topP        bool
	stop        bool
}

// vendorErrorEnvelope is the subset of a vendor error response body this
// package parses for its OWN internal retry heuristic — Message is used only
// for phrase-matching (never written to a response body or log line
// verbatim, since it may carry account/request detail); Param is the
// vendor's own structured field naming the offending parameter, checked
// first since it needs no phrase-matching at all.
type vendorErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Param   string `json:"param"`
	} `json:"error"`
}

// samplingUnsupportedPhrases mirrors internal/provider/anthropic's own
// message-based heuristic phrase list, kept package-local (rather than
// shared) since this package additionally has a structured `param` field to
// check first, which Anthropic's vendor error body never sends.
var samplingUnsupportedPhrases = []string{
	"not supported", "unsupported", "deprecated", "does not support", "is not compatible",
}

// detectUnsupportedParam inspects a 400 response body for a parameter this
// package can retry without: the vendor's structured "param" field if it
// names temperature, top_p, or stop, else a phrase-based match against the
// message text against the same three names. already is the set of params
// already dropped on a prior attempt (always empty on the first attempt,
// since Complete/Stream retry at most once); the returned droppedParams
// merges in anything newly detected. ok reports whether anything NEW was
// found — false means the 400 is unrelated to a sampling parameter (or names
// one already dropped), and the caller must not retry again.
func detectUnsupportedParam(body []byte, already droppedParams) (next droppedParams, ok bool) {
	var envelope vendorErrorEnvelope
	if json.Unmarshal(body, &envelope) != nil {
		return already, false
	}

	next = already
	param := strings.ToLower(strings.TrimSpace(envelope.Error.Param))
	msg := strings.ToLower(envelope.Error.Message)

	// A 400 the vendor attributes to some other field (response_format,
	// max_completion_tokens, ...) is never a sampling-parameter problem, so
	// it must not be retried by dropping one just because the message text
	// happens to contain a matching word.
	if param != "" && param != "temperature" && param != "top_p" && param != "stop" {
		return already, false
	}

	mark := func(name string, dropped *bool) {
		if *dropped {
			return
		}
		if param == name || messageNamesParamUnsupported(msg, name) {
			*dropped = true
			ok = true
		}
	}
	mark("temperature", &next.temperature)
	mark("top_p", &next.topP)
	mark("stop", &next.stop)

	return next, ok
}

// messageNamesParamUnsupported reports whether msg (already lowercased)
// mentions param alongside one of samplingUnsupportedPhrases.
func messageNamesParamUnsupported(msg, param string) bool {
	if msg == "" || !strings.Contains(msg, param) {
		return false
	}
	for _, phrase := range samplingUnsupportedPhrases {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}
