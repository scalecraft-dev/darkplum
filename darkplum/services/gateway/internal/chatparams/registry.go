// Package chatparams is the contract for every OpenAI chat-completions request
// parameter on every backend: one registry, one disposition per
// (parameter, value class, backend). The chat handler decides what to do with
// a request parameter by looking it up here -- there is no second table -- and
// docs/chat-params.json is a golden copy of this registry that a test pins in
// both directions, so the contract a caller reads cannot drift from what the
// gateway does.
//
// Dispositions (what happens to a parameter the caller sent, per backend):
//
//	forward         sent to the backend as is
//	translate       mapped to the backend's equivalent
//	accept-default  the caller sent the backend's default value; silent and
//	                not recorded (null for any key is the same)
//	drop-recorded   not sent; named in X-Darkplum-Dropped-Params and in
//	                inference_events.dropped_params. In an organisation's
//	                strict mode it is refused (400 unsupported_parameter)
//	                instead
//	contract-refuse 400 unsupported_parameter naming the field, in every mode,
//	                because silently dropping it would hand the caller a wrong
//	                answer
//
// A row is a (Param, When) pair: When names the value class the disposition
// applies to ("default", "non-default", "true", "with-tools", ...), because
// the same parameter is usually honoured for one value and refused for
// another. Nested keys of stream_options and response_format are rows of
// their own, named "parent.child".
//
// The mock backend is deliberately not in the registry: it is a development
// stand-in that answers canned text, no customer traffic reaches it, and it
// is dispatched with the openaicompat column. Any other unknown provider kind
// gets the same column.
package chatparams

import "strings"

// Backend is a backend class in the contract.
type Backend string

const (
	OpenAI       Backend = "openai"
	OpenAICompat Backend = "openaicompat" // in-cluster vLLM and LM Studio
	Anthropic    Backend = "anthropic"
	Bedrock      Backend = "bedrock"
)

// Backends is the column order everywhere (registry, golden, docs).
var Backends = []Backend{OpenAI, OpenAICompat, Anthropic, Bedrock}

// BackendOfKind maps a route's provider kind to its registry column. exact is
// false for a kind with no column of its own (mock, anything unknown), which
// is given the openaicompat column.
func BackendOfKind(kind string) (b Backend, exact bool) {
	switch kind {
	case "openai":
		return OpenAI, true
	case "vllm", "lmstudio":
		return OpenAICompat, true
	case "anthropic":
		return Anthropic, true
	case "bedrock":
		return Bedrock, true
	}
	return OpenAICompat, false
}

// Disposition is what happens to a parameter on a backend.
type Disposition string

const (
	Forward        Disposition = "forward"
	Translate      Disposition = "translate"
	AcceptDefault  Disposition = "accept-default"
	DropRecorded   Disposition = "drop-recorded"
	ContractRefuse Disposition = "contract-refuse"
)

// Dispositions lists the five in documentation order.
var Dispositions = []Disposition{Forward, Translate, AcceptDefault, DropRecorded, ContractRefuse}

// Row is one registry entry.
type Row struct {
	// Param is the request field ("seed") or a nested one
	// ("stream_options.include_usage").
	Param string `json:"param"`
	// When names the value class this row applies to; see the package doc.
	When string `json:"when"`
	// Backends is the disposition per backend, in default (lenient) mode.
	Backends map[Backend]Disposition `json:"backends"`
	// Strict is the disposition per backend in an organisation's strict mode:
	// identical to Backends except that drop-recorded becomes
	// contract-refuse.
	Strict map[Backend]Disposition `json:"strict"`
	// Dynamic marks a row whose outcome the handler decides per request or
	// per model (reasoning_effort, thinking side effects, the user
	// pseudonym) rather than from the value alone; Backends is then the
	// normal outcome and Note says when it differs.
	Dynamic bool `json:"dynamic,omitempty"`
	// Early marks a refusal that is the same on every backend and is decided
	// before the model is resolved.
	Early bool `json:"before_route,omitempty"`
	// RefusalParam is the field a refusal names when it is not Param.
	RefusalParam string `json:"refusal_param,omitempty"`
	// Reason is the sentence a contract refusal appends to the message.
	Reason string `json:"reason,omitempty"`
	// Note is a short explanation rendered into the API docs.
	Note string `json:"note,omitempty"`
}

// For returns the row's disposition on b.
func (r Row) For(b Backend) Disposition { return r.Backends[b] }

// RefusalName is the field a contract refusal of this row names.
func (r Row) RefusalName() string {
	if r.RefusalParam != "" {
		return r.RefusalParam
	}
	return r.Param
}

// CatchAll is the rule for a request key the registry does not name. It is
// part of the golden file, but it is a rule, not a row: it is tested as a
// rule in both modes.
type CatchAll struct {
	Lenient Disposition `json:"lenient"`
	Strict  Disposition `json:"strict"`
	Note    string      `json:"note"`
}

// UnknownKeys is the catch-all for unknown top-level keys and unknown nested
// keys of stream_options and response_format.
var UnknownKeys = CatchAll{
	Lenient: DropRecorded,
	Strict:  ContractRefuse,
	Note:    "A key the gateway does not know -- at the top level of the body, or inside stream_options or response_format -- is dropped and recorded under its own name; in strict mode it is refused. A null value is silent.",
}

// MockExcluded is the stated exclusion of the mock backend.
const MockExcluded = "The mock backend is a development stand-in that returns canned text and receives no customer traffic. It has no column here and is dispatched with the openaicompat column."

// code letters used by the table below, in Backends order.
var letter = map[byte]Disposition{
	'F': Forward, 'T': Translate, 'A': AcceptDefault, 'D': DropRecorded, 'C': ContractRefuse,
}

// row builds a Row from four letters (openai, openaicompat, anthropic,
// bedrock).
func row(param, when, cells string, opts ...func(*Row)) Row {
	if len(cells) != len(Backends) {
		panic("chatparams: bad cells for " + param)
	}
	r := Row{Param: param, When: when, Backends: map[Backend]Disposition{}, Strict: map[Backend]Disposition{}}
	for i, b := range Backends {
		d, ok := letter[cells[i]]
		if !ok {
			panic("chatparams: bad letter in " + param)
		}
		r.Backends[b] = d
		if d == DropRecorded {
			d = ContractRefuse
		}
		r.Strict[b] = d
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func note(s string) func(*Row)    { return func(r *Row) { r.Note = s } }
func reason(s string) func(*Row)  { return func(r *Row) { r.Reason = s } }
func refusal(s string) func(*Row) { return func(r *Row) { r.RefusalParam = s } }
func dynamic(r *Row)              { r.Dynamic = true }
func early(r *Row)                { r.Early = true }

// Value classes used in When. "any" means every non-null value is treated
// alike.
const (
	WhenAny        = "any"
	WhenDefault    = "default"
	WhenNonDefault = "non-default"
)

// rows is ordered: refusals are reported in this order when a request has
// several problems, so the same request always gets the same answer.
var rows = []Row{
	// --- refused on every backend: checked before the model is resolved ---
	row("functions", WhenNonDefault, "CCCC", refusal("functions"),
		early,
		reason("the legacy functions API is not implemented; send tools"),
		note("Legacy functions API. An empty array or null is the default.")),
	row("functions", WhenDefault, "AAAA"),
	row("function_call", WhenNonDefault, "CCCC",
		early,
		reason("the legacy functions API is not implemented; send tool_choice"),
		note("Anything other than \"none\" or null.")),
	row("function_call", WhenDefault, "AAAA"),
	row("response_format", "json_schema", "CCCC",
		early,
		reason("structured JSON output is not implemented on any backend"),
		note("Structured outputs.")),
	row("response_format", "unknown-type", "CCCC",
		early,
		reason("response_format type must be text or json_object"),
		note("A type other than text, json_object or json_schema.")),
	row("messages", "non-text-content", "CCCC", refusal("messages[].content"),
		early,
		reason("images, audio and files in message content are not supported"),
		note("A content array containing anything but text parts.")),

	// --- routing and shape ---
	row("model", WhenAny, "FFFF", note("An alias resolves to a concrete model id first.")),
	row("messages", "text", "FFTT", note("Translated to the backend's own message schema where it differs.")),
	row("stream", WhenAny, "FFTT"),
	row("stream_options", "object", "FFFF", note("Container: its keys are the rows below. Without stream: true it is a 400 invalid_stream_options.")),
	row("stream_options.include_usage", "true", "FFTT",
		note("Anthropic and Bedrock report usage differently; the gateway ends the stream with OpenAI's usage chunk (choices: [])."),
		reason("a usage chunk cannot be produced on this backend")),
	row("stream_options.include_usage", WhenDefault, "AAAA", note("false.")),
	row("stream_options.include_obfuscation", WhenAny, "DDDD", note("OpenAI-specific stream padding; the gateway does not forward it.")),
	row("max_tokens", WhenAny, "TFTT", note("OpenAI receives it as max_completion_tokens.")),
	row("max_tokens", "with-thinking", "FFTT", dynamic,
		note("With reasoning_effort translated to extended thinking the value sent is the caller's plus the thinking budget, capped at the model limit; the change is reported as max_tokens=raised.")),
	row("max_completion_tokens", WhenAny, "FTTT", note("The same limit as max_tokens under its newer name.")),
	row("max_completion_tokens", "conflicts-with-max_tokens", "CCCC",
		early,
		reason("max_tokens and max_completion_tokens are the same limit and were sent with different values; send one"),
		note("Both sent and different.")),

	// --- sampling ---
	row("temperature", WhenAny, "FFFF"),
	row("temperature", "with-thinking", "FFDD", dynamic,
		note("Extended thinking forbids a caller temperature; it is dropped when reasoning_effort becomes thinking.")),
	row("top_p", WhenAny, "FFFF"),
	row("top_p", "with-thinking", "FFDD", dynamic,
		note("A top_p below 0.95 is dropped when reasoning_effort becomes extended thinking.")),
	row("seed", WhenAny, "FFDD", note("Dropping it removes reproducibility: the same request can return different text. Anthropic and Bedrock have no seed.")),
	row("frequency_penalty", WhenDefault, "AAAA", note("0.")),
	row("frequency_penalty", WhenNonDefault, "FFDD", note("Dropping it changes the output: the model repeats itself more.")),
	row("presence_penalty", WhenDefault, "AAAA", note("0.")),
	row("presence_penalty", WhenNonDefault, "FFDD", note("Dropping it changes the output.")),
	row("n", WhenDefault, "AAAA", note("1.")),
	row("n", WhenNonDefault, "CCCC", reason("only one choice per request is supported"), note("Anything but 1.")),
	row("stop", WhenAny, "FFTT", note("Anthropic stop_sequences, Bedrock stopSequences.")),
	row("logit_bias", WhenDefault, "AAAA", note("Empty.")),
	row("logit_bias", WhenNonDefault, "FFCC", reason("this backend does not apply logit_bias")),
	row("logprobs", WhenDefault, "AAAA", note("false.")),
	row("logprobs", "true", "FFCC", reason("this backend does not return logprobs")),
	row("top_logprobs", WhenDefault, "AAAA", note("0.")),
	row("top_logprobs", WhenNonDefault, "FFCC", reason("this backend does not return logprobs"),
		note("Requires logprobs: true, which a backend that cannot return logprobs refuses first.")),

	// --- tools ---
	row("tools", WhenDefault, "AAAA", note("Empty array.")),
	row("tools", "non-empty", "FFTT", note("Translated to Anthropic tools and Bedrock toolConfig.")),
	row("tool_choice", "auto-or-none", "FFTT"),
	row("tool_choice", "required-or-named", "FFTT",
		reason("this backend cannot enforce a required or named tool call"),
		note("A refusal is the answer for a backend that cannot enforce it; every current backend can.")),
	row("parallel_tool_calls", WhenDefault, "AAAA", note("true.")),
	row("parallel_tool_calls", "false-with-tools", "FCTC",
		reason("this backend cannot restrict a turn to a single tool call"),
		note("Anthropic receives disable_parallel_tool_use. vLLM and Bedrock cannot enforce it.")),
	row("parallel_tool_calls", "false-without-tools", "AAAA", note("Nothing to restrict without tools.")),

	// --- output format ---
	row("response_format", "text", "AAAA", note("The default.")),
	row("response_format", "json_object", "FFCC", reason("this backend has no JSON mode")),
	row("response_format.type", WhenAny, "FFFF", note("Selects the response_format row above.")),
	row("response_format.json_schema", WhenAny, "DDDD", note("Only meaningful with type json_schema, which is refused before this is reached.")),

	// --- identity, storage, reasoning ---
	row("user", WhenAny, "TFTT", dynamic,
		note("In-cluster vLLM receives the value as sent. OpenAI (user), Anthropic (metadata.user_id) and Bedrock (requestMetadata) receive a keyed HMAC, never the raw value. If the HMAC secret is not installed it is dropped and recorded.")),
	row("store", WhenDefault, "FAAA", note("false, or null. OpenAI receives store: false.")),
	row("store", "true", "CCCC", reason("completions are never stored by a vendor on this gateway's behalf; send store:false or omit it"),
		note("true.")),
	row("reasoning_effort", "none", "FAAA", dynamic, note("No reasoning requested; nothing is sent or recorded except to OpenAI, where none is a real setting.")),
	row("reasoning_effort", "other", "FDTT", dynamic,
		note("OpenAI forwards it. Anthropic and Anthropic models on Bedrock translate it to extended thinking (low and minimal 1024, medium 4096, high and xhigh 16384 budget tokens); it is dropped and recorded for a model without extended thinking and on any turn after a tool call, and refused with a required or named tool_choice. Other backends drop it.")),

	// --- OpenAI parameters the gateway does not implement ---
	row("metadata", WhenDefault, "AAAA", note("Empty object.")),
	row("metadata", WhenNonDefault, "DDDD"),
	row("service_tier", WhenDefault, "AAAA", note("auto or default.")),
	row("service_tier", WhenNonDefault, "DDDD"),
	row("modalities", WhenDefault, "AAAA", note("[\"text\"].")),
	row("modalities", WhenNonDefault, "CCCC", reason("audio output is not produced by any backend; the reply is text only"),
		note("Anything but [\"text\"], i.e. a request for audio output.")),
	row("audio", WhenAny, "CCCC", reason("audio output is not produced by any backend")),
	row("prediction", WhenAny, "DDDD"),
	row("web_search_options", WhenAny, "CCCC", reason("web search is not available on any backend; the model would answer without it"),
		note("A request for web search is refused rather than answered without it.")),
	row("verbosity", WhenDefault, "AAAA", note("medium.")),
	row("verbosity", WhenNonDefault, "DDDD"),
	row("safety_identifier", WhenAny, "DDDD"),
	row("prompt_cache_key", WhenAny, "DDDD"),
	row("prompt_cache_retention", WhenAny, "DDDD"),

	// --- vLLM extensions: only in-cluster vLLM understands them ---
	row("top_k", WhenAny, "DFDD", note("Anthropic has a top_k of its own but the gateway does not translate it.")),
	row("min_p", WhenAny, "DFDD"),
	row("repetition_penalty", WhenAny, "DFDD"),
	row("guided_json", WhenAny, "DFDD"),
	row("chat_template_kwargs", WhenAny, "DFDD", note("Only enable_thinking, thinking and reasoning_effort, scalar values.")),
}

// Rows returns the registry in order. The slice is shared; do not modify it.
func Rows() []Row { return rows }

// Lookup returns the row for (param, when).
func Lookup(param, when string) (Row, bool) {
	for _, r := range rows {
		if r.Param == param && r.When == when {
			return r, true
		}
	}
	return Row{}, false
}

// ParamKnown reports whether any row names param (top-level or nested).
func ParamKnown(param string) bool {
	for _, r := range rows {
		if r.Param == param {
			return true
		}
	}
	return false
}

// TopLevel reports whether key is a request key the registry knows: a row's
// parameter, or the parent of a nested row.
func TopLevel(key string) bool {
	for _, r := range rows {
		p := r.Param
		if i := strings.IndexByte(p, '.'); i >= 0 {
			p = p[:i]
		}
		if p == key {
			return true
		}
	}
	return false
}

// RefusedEverywhere reports whether the row is a contract refusal on every
// backend, i.e. one the handler can decide before it has resolved a model.
func (r Row) RefusedEverywhere() bool {
	for _, b := range Backends {
		if r.Backends[b] != ContractRefuse {
			return false
		}
	}
	return true
}

// ParamNames is every distinct parameter name in the registry: the bounded set
// whose first-drop audit is never suppressed.
func ParamNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if !seen[r.Param] {
			seen[r.Param] = true
			out = append(out, r.Param)
		}
	}
	return out
}
