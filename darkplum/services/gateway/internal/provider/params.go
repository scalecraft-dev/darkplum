package provider

// ParamCaps states which chat-completions parameters a backend can honour
// faithfully. The handler consults it after route resolution and refuses
// (400 unsupported_parameter, naming the field) a request that asks for one
// the backend cannot honour, rather than letting the provider silently drop
// it: a dropped response_format or logit_bias is a wrong answer, not a
// degraded one. A capability is true only when the provider actually puts the
// parameter on the wire (or translates it), so a later change that starts
// forwarding a parameter flips its flag in the same diff.
//
// Parameters at their default value (n:1, logprobs:false, an empty
// logit_bias, parallel_tool_calls:true, response_format text, null) need no
// capability: the handler accepts them silently on every backend.
type ParamCaps struct {
	// JSONMode: response_format {"type":"json_object"} is forwarded.
	JSONMode bool
	// Stop: stop sequences are forwarded.
	Stop bool
	// Logprobs: logprobs:true is forwarded and the logprobs returned.
	Logprobs bool
	// LogitBias: a non-empty logit_bias is forwarded.
	LogitBias bool
	// ForcedToolChoice: tool_choice "required" or a named function is
	// enforced by the backend.
	ForcedToolChoice bool
	// SerialToolCalls: parallel_tool_calls:false is enforced by the backend.
	SerialToolCalls bool
	// UserID says what the backend is given for the caller's `user` field.
	// The handler fills ChatRequest.User according to it, so a provider only
	// ever forwards ChatRequest.User and can never see the raw value of a
	// field it must pseudonymise.
	UserID UserIDMode
}

// UserIDMode is how a backend receives the OpenAI `user` field.
type UserIDMode int

const (
	// UserIDNone: the backend has nowhere to put it; the field is not sent
	// (today's behaviour, unchanged).
	UserIDNone UserIDMode = iota
	// UserIDRaw: forwarded verbatim. Only for a backend inside the customer's
	// boundary (in-cluster vLLM / LM Studio).
	UserIDRaw
	// UserIDHMAC: sent as <id>:<hex> HMAC-SHA256 under the org's secret
	// (OpenAI user, Anthropic metadata.user_id, Bedrock requestMetadata); all
	// three receive the same value for the same person. Those fields
	// are free-form but Anthropic documents that they must not carry an
	// email address or name, and Bedrock writes request metadata to
	// invocation logs; the raw value never leaves the gateway.
	UserIDHMAC
)

// ParamCapable is implemented by a provider that reports its ParamCaps. A
// provider that does not implement it is treated as supporting none of them.
type ParamCapable interface {
	ParamCaps() ParamCaps
}

// CapsOf returns p's ParamCaps, the zero value when p does not report any.
func CapsOf(p Provider) ParamCaps {
	if pc, ok := p.(ParamCapable); ok {
		return pc.ParamCaps()
	}
	return ParamCaps{}
}
