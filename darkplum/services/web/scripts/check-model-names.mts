/**
 * Model ids -> display names, for every shape the catalogue holds.
 *
 * Run: node --experimental-strip-types scripts/check-model-names.mts
 *      (pnpm run check:names).
 */
import { disambiguate, formatModelName, parseBedrockId } from '../src/lib/formatModelName.ts'

let failures = 0
function check(name: string, got: unknown, want: unknown) {
  if (JSON.stringify(got) === JSON.stringify(want)) return
  failures++
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`)
}

// Bedrock inference profiles and foundation models, as the live catalogue has them.
const bedrock: [string, string][] = [
  ['us.anthropic.claude-sonnet-4-5-20250929-v1:0', 'Claude Sonnet 4.5'],
  ['global.anthropic.claude-sonnet-4-5-20250929-v1:0', 'Claude Sonnet 4.5'],
  ['global.anthropic.claude-haiku-4-5-20251001-v1:0', 'Claude Haiku 4.5'],
  ['us.anthropic.claude-opus-4-1-20250805-v1:0', 'Claude Opus 4.1'],
  ['global.anthropic.claude-opus-4-6-v1', 'Claude Opus 4.6'],
  ['us.anthropic.claude-opus-4-7', 'Claude Opus 4.7'],
  ['global.anthropic.claude-fable-5-1', 'Claude Fable 5.1'],
  ['global.anthropic.claude-fable-5', 'Claude Fable 5'],
  ['us.anthropic.claude-3-haiku-20240307-v1:0', 'Claude 3 Haiku'],
  ['us.anthropic.claude-3-5-sonnet-20241022-v2:0', 'Claude 3.5 Sonnet'],
  ['us.amazon.nova-pro-v1:0', 'Nova Pro'],
  ['global.amazon.nova-2-lite-v1:0', 'Nova 2 Lite'],
  ['us.amazon.nova-micro-v1:0', 'Nova Micro'],
  ['us.meta.llama3-2-90b-instruct-v1:0', 'Llama 3.2 90B'],
  ['us.meta.llama3-2-1b-instruct-v1:0', 'Llama 3.2 1B'],
  ['us.meta.llama4-maverick-17b-instruct-v1:0', 'Llama 4 Maverick 17B'],
  ['openai.gpt-oss-120b-1:0', 'GPT OSS 120B'],
  ['us.deepseek.r1-v1:0', 'R1'],
  ['us.mistral.pixtral-large-2502-v1:0', 'Pixtral Large 2502'],
  ['cohere.embed-v4:0', 'Embed v4'],
  ['us.xai.grok-4-1-fast', 'Grok 4.1 Fast'],
  ['us.moonshotai.kimi-k2-thinking', 'Kimi K2 Thinking'],
  ['stability.sd3-5-large-v1:0', 'SD3.5 Large'],
]
for (const [id, want] of bedrock) check(id, formatModelName(id, 'bedrock'), want)
for (const [id, want] of bedrock) check(`${id} (provider unknown)`, formatModelName(id), want)

// Vendor-API ids read the same as their Bedrock twins.
check('anthropic direct', formatModelName('claude-sonnet-4-5-20250929', 'anthropic'), 'Claude Sonnet 4.5')
check('openai direct', formatModelName('gpt-4.1-mini', 'openai'), 'GPT 4.1 Mini')
check('openai o-series', formatModelName('o4-mini', 'openai'), 'O4 Mini')

// Self-hosted ids keep the hosted rule.
check('hf path', formatModelName('meta-llama/Llama-3.1-8B-Instruct', 'vllm'), 'Llama 3.1 8B')
check('hf path, provider unknown', formatModelName('meta-llama/Llama-3.1-8B-Instruct'), 'Llama 3.1 8B')
check('mock', formatModelName('mock-model', 'mock'), 'Mock Model')

// Parsing.
check('parse scoped', parseBedrockId('us.anthropic.claude-opus-4-7'), {
  scope: 'us',
  maker: 'anthropic',
  unscoped: 'anthropic.claude-opus-4-7',
  tail: 'claude-opus-4-7',
})
check('parse foundation', parseBedrockId('cohere.embed-v4:0'), {
  scope: null,
  maker: 'cohere',
  unscoped: 'cohere.embed-v4:0',
  tail: 'embed-v4:0',
})
check('parse plain', parseBedrockId('gpt-4.1'), { scope: null, maker: null, unscoped: 'gpt-4.1', tail: 'gpt-4.1' })

// Collisions within a group get the id's date; unique names stay clean.
const d = disambiguate([
  'us.anthropic.claude-3-5-sonnet-20240620-v1:0',
  'us.anthropic.claude-3-5-sonnet-20241022-v2:0',
  'us.anthropic.claude-sonnet-4-5-20250929-v1:0',
])
check('collision a', d.get('us.anthropic.claude-3-5-sonnet-20240620-v1:0'), 'Claude 3.5 Sonnet (Jun 2024)')
check('collision b', d.get('us.anthropic.claude-3-5-sonnet-20241022-v2:0'), 'Claude 3.5 Sonnet (Oct 2024)')
check('no collision', d.get('us.anthropic.claude-sonnet-4-5-20250929-v1:0'), 'Claude Sonnet 4.5')

if (failures > 0) {
  console.error(`${failures} check(s) failed`)
  process.exit(1)
}
console.log('model names OK')
