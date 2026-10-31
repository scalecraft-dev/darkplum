package main

import (
	"fmt"
	"strings"
)

// paramsDoc mirrors docs/chat-params.json, which the gateway writes from its
// parameter registry and pins with a test. This command reads the file only:
// there is no Go import between the services.
type paramsDoc struct {
	Backends     []string `json:"backends"`
	Dispositions []string `json:"dispositions"`
	Excluded     struct {
		Mock string `json:"mock"`
	} `json:"excluded"`
	UnknownKeys struct {
		Lenient string `json:"lenient"`
		Strict  string `json:"strict"`
		Note    string `json:"note"`
	} `json:"unknown_keys"`
	Rows []paramsRow `json:"rows"`
}

type paramsRow struct {
	Param    string            `json:"param"`
	When     string            `json:"when"`
	Backends map[string]string `json:"backends"`
	Strict   map[string]string `json:"strict"`
	Dynamic  bool              `json:"dynamic"`
	Note     string            `json:"note"`
}

var backendHeadings = map[string]string{
	"openai": "OpenAI", "openaicompat": "vLLM / LM Studio", "anthropic": "Anthropic", "bedrock": "Bedrock",
}

// chatParamsIntro is the prose above the generated table.
const chatParamsIntro = "### Chat parameters\n\n" +
	"Every OpenAI `POST /v1/chat/completions` request parameter has one of five dispositions per backend. The table below is generated from `docs/chat-params.json`, the gateway's own registry (a gateway test fails if the two differ), so it is what the gateway does, not a description of intent.\n\n" +
	"- **forward**: sent to the backend as is. **translate**: mapped to the backend's equivalent.\n" +
	"- **accept-default**: you sent the backend's default value (`n: 1`, penalties `0`, an empty `logit_bias`, `store: false`, `parallel_tool_calls: true`, `logprobs: false`, `service_tier` `auto`, `response_format` `text`) or `null` for any key. Silent and not recorded.\n" +
	"- **drop-recorded**: not sent. Named in the `X-Darkplum-Dropped-Params` response header and in the usage log, and audited the first time it happens for a key.\n" +
	"- **contract-refuse**: `400` `unsupported_parameter` with `param` naming the field, in every mode, because dropping it would hand you a wrong answer (a JSON-mode request answered in prose, three choices collapsed to one, a forced tool call that was not forced).\n\n" +
	"**Lenient is the default.** A parameter a backend cannot honour that is not a contract refusal is dropped and recorded rather than failing the request, so an SDK that sends its usual defaults keeps working on every backend. An unknown top-level key (or an unknown key inside `stream_options` or `response_format`) is dropped and recorded under its own name. **Strict mode** is an organisation-wide switch (`params_mode` on `PUT /api/v1/routing-policy`): every drop becomes a `400` `unsupported_parameter` instead, naming the first parameter; contract refusals are unchanged. Dropping is not free: dropping `seed` means the same request can return different text, and dropping `frequency_penalty`, `presence_penalty` or a sampling parameter changes the output.\n\n" +
	"**Where drops are visible.** (1) `X-Darkplum-Dropped-Params` on the response, decided before the first byte so it is on a stream too: comma-separated, a bare name for a dropped parameter (`seed`, `stream_options.include_obfuscation`), `name=raised` for an adjustment (`max_tokens=raised`: extended thinking needed more room than the limit you set), at most 20 unknown key names and then `unknown_keys=more_N`. Only names are recorded, never the values you sent; but an unknown key's name is chosen by the caller and could itself carry data, so treat it as data in your own account (it never leaves it). (2) The usage log, the same entries: `dropped_params` on each `GET /api/v1/usage` row (an array of strings, `null` when nothing was dropped) and the `dropped_params` column of `GET /api/v1/usage/export` (entries joined with `;`, empty when nothing was dropped); the Activity screen badges such a request. (3) The audit log: one `inference.param_dropped` event the first time each API key (or, for webchat, each user) has a given parameter dropped, never repeated per request.\n\n" +
	"A parameter is dropped for `reasoning_effort` too where the backend has no equivalent, for a Claude model without extended thinking, and on any turn after a tool call; with extended thinking on, `temperature`, a `top_p` below 0.95 and `top_k` are dropped and `max_tokens` is raised. `user` is dropped (recorded) when the HMAC secret is not installed, and is never sent raw to a vendor; in strict mode that is a `400` (for example on the dev overlay, which installs no secret). A request for audio output (`modalities` other than `[\"text\"]`, or `audio`) or for web search (`web_search_options`) is a contract refusal, not a drop.\n\n" +
	"**Rollout.** Strict mode only takes effect on gateways running this release. Until migration 029 is applied the gateway reads every organisation as lenient (and logs a warning), and `darkplum deploy` rolls the api and then the gateway in one run, so a strict setting made before the gateway has rolled is ignored by the old gateways. Check `X-Darkplum-Dropped-Params` on a request with an unknown key to confirm the gateway you reach is the new one.\n\n"

func renderChatParams(d *paramsDoc) string {
	var b strings.Builder
	b.WriteString(chatParamsIntro)
	b.WriteString("| Parameter | Value | ")
	for _, be := range d.Backends {
		b.WriteString(backendHeadings[be] + " | ")
	}
	b.WriteString("Strict mode |\n|---|---|")
	for range d.Backends {
		b.WriteString("---|")
	}
	b.WriteString("---|\n")
	for _, r := range d.Rows {
		name := "`" + r.Param + "`"
		if r.Dynamic {
			name += " *"
		}
		fmt.Fprintf(&b, "| %s | %s |", name, r.When)
		var refused []string
		for _, be := range d.Backends {
			fmt.Fprintf(&b, " %s |", r.Backends[be])
			if r.Strict[be] != r.Backends[be] {
				refused = append(refused, backendHeadings[be])
			}
		}
		if len(refused) == 0 {
			b.WriteString(" same |\n")
		} else {
			fmt.Fprintf(&b, " refused on %s |\n", strings.Join(refused, ", "))
		}
	}
	b.WriteString("\n")
	b.WriteString("`*` marks a row the gateway decides per request or per model rather than from the value alone; the table gives the normal outcome. ")
	b.WriteString(d.UnknownKeys.Note + " ")
	b.WriteString(d.Excluded.Mock + "\n\n")
	var notes []string
	for _, r := range d.Rows {
		if r.Note != "" {
			notes = append(notes, fmt.Sprintf("- `%s` (%s): %s", r.Param, r.When, r.Note))
		}
	}
	b.WriteString("Notes by row:\n\n" + strings.Join(notes, "\n") + "\n\n")
	return b.String()
}
