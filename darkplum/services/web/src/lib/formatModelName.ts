/**
 * A readable name for a model id, where the catalogue has not been given one.
 *
 * Two shapes arrive here. Self-hosted ids look like a Hugging Face path
 * ("meta-llama/Llama-3.1-8B-Instruct"); Bedrock inference-profile ids look like
 * "us.anthropic.claude-sonnet-4-5-20250929-v1:0", where the first segment is
 * the routing scope, the second the maker, and the tail a revision. Vendor-API
 * ids ("claude-sonnet-4-5-20250929") are the Bedrock tail without the prefixes.
 *
 * The maker is never put in the name: the catalogue groups by maker already,
 * and "Claude Sonnet 4.5" under an Anthropic heading says it once.
 */

/** Bedrock cross-region inference scopes. */
export const BEDROCK_SCOPES = ['us', 'eu', 'apac', 'global', 'us-gov', 'jp', 'au', 'ca'] as const
export type BedrockScope = (typeof BEDROCK_SCOPES)[number]

/** Makers whose ids are known to take the Bedrock shape "maker.model". */
const BEDROCK_MAKERS = new Set([
  'ai21',
  'amazon',
  'anthropic',
  'cohere',
  'deepseek',
  'meta',
  'mistral',
  'moonshotai',
  'openai',
  'qwen',
  'stability',
  'twelvelabs',
  'writer',
  'xai',
  'zai',
])

/** Ids where "-vN" is the product's own version, not Bedrock's revision. */
const EXCEPTIONS: Record<string, string> = {
  'cohere.embed-v4:0': 'Embed v4',
  'cohere.embed-english-v3': 'Embed English v3',
  'cohere.embed-multilingual-v3': 'Embed Multilingual v3',
}

/** Tokens whose case the title-case pass would get wrong. */
const CASING: Record<string, string> = {
  gpt: 'GPT',
  oss: 'OSS',
  glm: 'GLM',
  r1: 'R1',
  k2: 'K2',
  sd3: 'SD3',
  'sd3.5': 'SD3.5',
  x5: 'X5',
  ai: 'AI',
  xl: 'XL',
  sdxl: 'SDXL',
  ocr: 'OCR',
  tts: 'TTS',
  vl: 'VL',
  moe: 'MoE',
  llm: 'LLM',
}

const SCOPE_RE = new RegExp(`^(${BEDROCK_SCOPES.join('|')})\\.`)

export interface ParsedBedrockId {
  /** "us", "global", … or null for a foundation-model id with no scope. */
  scope: BedrockScope | null
  /** "anthropic", "amazon", … or null when the id has no maker segment. */
  maker: string | null
  /** The id with the scope prefix removed: what the same model is called in
   *  every scope it is offered in. */
  unscoped: string
  /** The id with scope, maker and Bedrock revision removed. */
  tail: string
}

/** Splits a Bedrock id into its parts. Non-Bedrock ids come back with no
 *  scope and no maker, and the whole id as the tail. */
export function parseBedrockId(id: string): ParsedBedrockId {
  let rest = id
  let scope: BedrockScope | null = null
  const s = rest.match(SCOPE_RE)
  if (s) {
    scope = s[1] as BedrockScope
    rest = rest.slice(s[0].length)
  }
  const unscoped = rest
  let maker: string | null = null
  const m = rest.match(/^([a-z0-9]+)\.(.+)$/)
  if (m && BEDROCK_MAKERS.has(m[1])) {
    maker = m[1]
    rest = m[2]
  }
  return { scope, maker, unscoped, tail: rest }
}

/** What a routing scope is called on screen. */
export function scopeLabel(scope: BedrockScope | null): string {
  switch (scope) {
    case 'us':
      return 'US'
    case 'eu':
      return 'EU'
    case 'apac':
      return 'APAC'
    case 'global':
      return 'Global'
    case 'us-gov':
      return 'US Gov'
    case 'jp':
      return 'Japan'
    case 'au':
      return 'Australia'
    case 'ca':
      return 'Canada'
    default:
      return 'Direct'
  }
}

export function isBedrockId(id: string): boolean {
  const p = parseBedrockId(id)
  return p.maker !== null || p.scope !== null
}

function titleToken(tok: string): string {
  const lower = tok.toLowerCase()
  if (CASING[lower]) return CASING[lower]
  // A parameter count: 90b, 8b, 1.5b, 400m, 32k.
  if (/^\d+(\.\d+)?[bmk]$/.test(lower)) return lower.toUpperCase()
  return lower.charAt(0).toUpperCase() + lower.slice(1)
}

/** "claude-sonnet-4-5-20250929-v1:0" -> "Claude Sonnet 4.5". Also the right
 *  thing for a vendor-API id, which is this tail with no prefixes. */
function formatBedrockTail(tail: string): { name: string; date: string | null } {
  let t = tail
  // Bedrock's revision: "-v1:0", "-1:0", ":0".
  t = t.replace(/(-v\d+)?(-\d+)?:\d+$/, '')
  // A trailing "-v1" with no ":0" (some profile ids drop the version suffix).
  t = t.replace(/-v\d+$/, '')
  let date: string | null = null
  t = t.replace(/-(\d{8})(?=-|$)/, (_, d: string) => {
    date = d
    return ''
  })
  t = t.replace(/-(instruct|chat)(?=-|$)/gi, '')
  // "llama3" -> "llama 3", "nova2" -> "nova 2", but "sd3" and "gpt4o" stay.
  const tokens = t
    .split(/[-_]+/)
    .filter(Boolean)
    .flatMap((tok) => {
      const m = tok.match(/^([a-z]{3,})(\d+)$/i)
      if (m && !CASING[tok.toLowerCase()]) return [m[1], m[2]]
      return [tok]
    })
  // Adjacent short numbers are a version: "4 5" -> "4.5", "3 2 90b" -> "3.2 90B".
  const out: string[] = []
  for (const tok of tokens) {
    const prev = out[out.length - 1]
    // "sd3" + "5" is a version too: a token that ends in a short number.
    if (prev !== undefined && /^(?:[a-z]*\d{1,2}|\d{1,2})$/i.test(prev) && /^\d{1,2}$/.test(tok)) {
      out[out.length - 1] = `${prev}.${tok}`
      continue
    }
    out.push(tok)
  }
  return { name: out.map(titleToken).join(' '), date }
}

/** "Oct 2024" from "20241022". */
function monthYear(date: string): string {
  const months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
  const mi = Number(date.slice(4, 6)) - 1
  return `${months[mi] ?? date.slice(4, 6)} ${date.slice(0, 4)}`
}

/** The name, with the date the id carries, so a caller that finds two ids
 *  naming the same thing can tell them apart (see disambiguate). */
export function formatBedrockModelName(id: string): { name: string; date: string | null } {
  const exception = EXCEPTIONS[parseBedrockId(id).unscoped]
  if (exception) return { name: exception, date: null }
  return formatBedrockTail(parseBedrockId(id).tail)
}

/** Adds the id's date to names that would otherwise read the same within a
 *  group: "Claude 3.5 Sonnet (Jun 2024)" next to "Claude 3.5 Sonnet (Oct 2024)". */
export function disambiguate(ids: string[]): Map<string, string> {
  const named = ids.map((id) => ({ id, ...formatBedrockModelName(id) }))
  const counts = new Map<string, number>()
  for (const n of named) counts.set(n.name, (counts.get(n.name) ?? 0) + 1)
  const out = new Map<string, string>()
  for (const n of named) {
    out.set(n.id, (counts.get(n.name) ?? 0) > 1 && n.date ? `${n.name} (${monthYear(n.date)})` : n.name)
  }
  return out
}

/** A self-hosted (Hugging Face style) id: "meta-llama/Llama-3.1-8B-Instruct"
 *  -> "Llama 3.1 8B". */
function formatHostedName(id: string): string {
  let name = id.includes('/') ? id.split('/').pop()! : id

  name = name
    .replace(/-instruct/gi, '')
    .replace(/-chat/gi, '')
    .replace(/-v\d+(\.\d+)*/gi, '')
    .replace(/-hf$/i, '')

  const sizeMatch = name.match(/(\d+)[bB]/i)
  const size = sizeMatch ? sizeMatch[1] + 'B' : null

  const family = name
    .replace(/[-_]\d+[bBx].*/, '')
    .replace(/[-_]/g, ' ')
    .replace(/\b\w/g, (c) => c.toUpperCase())
    .trim()

  return size ? `${family} ${size}` : family
}

/**
 * The display name for an id. `provider` decides the shape when known;
 * without it, an id that parses as Bedrock (a scope or a known maker prefix)
 * takes the Bedrock rule, and a vendor-API id (no slash, no maker, but a
 * Bedrock-style tail) does too, so "claude-sonnet-4-5-20250929" on Anthropic
 * direct reads the same as it does on Bedrock.
 */
export function formatModelName(id: string, provider?: string): string {
  if (provider === 'bedrock' || provider === 'anthropic' || provider === 'openai') {
    return formatBedrockModelName(id).name
  }
  if (provider === 'vllm' || provider === 'lmstudio') return formatHostedName(id)
  if (isBedrockId(id)) return formatBedrockModelName(id).name
  if (!id.includes('/') && /^[a-z0-9]+(-[a-z0-9.]+)+(:\d+)?$/i.test(id) && /\d/.test(id)) {
    return formatBedrockModelName(id).name
  }
  return formatHostedName(id)
}
