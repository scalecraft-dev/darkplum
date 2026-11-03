/**
 * How a chat stream ends, against the real reader.
 *
 * Run: node --experimental-strip-types scripts/check-chat-stream.mts
 *      (pnpm run check:stream).
 */
import { isCutOffReply, readChatStream } from '../src/lib/chatStream.ts'

const enc = new TextEncoder()
const tok = (t: string) => `data: {"choices":[{"delta":{"content":"${t}"}}]}\n\n`
const finish = 'data: {"choices":[{"delta":{},"finish_reason":"stop"}]}\n\n'
const done = 'data: [DONE]\n\n'
const errFrame = 'data: {"error":{"message":"m","code":"stream_interrupted"}}\n\n'

type Outcome = { end: string[]; text: string; tools: number }

async function run(parts: string[], fail?: Error): Promise<Outcome> {
  let i = 0
  const body = new ReadableStream<Uint8Array>({
    // One part per read: erroring a stream discards whatever is still queued.
    pull(c) {
      const p = parts[i++]
      if (p !== undefined) c.enqueue(enc.encode(p))
      else if (fail) c.error(fail)
      else c.close()
    },
  })
  const out: Outcome = { end: [], text: '', tools: 0 }
  await readChatStream(body.getReader(), {
    onToken: (t) => { out.text += t },
    onToolEvent: () => { out.tools++ },
    onDone: () => out.end.push('done'),
    onErrorFrame: (e) => out.end.push(`error:${e.code}`),
    onCutOff: () => out.end.push('cutoff'),
  })
  return out
}

const abort = new Error('aborted'); abort.name = 'AbortError'
const cases: Array<[string, string[], Error | undefined, string[], string]> = [
  ['[DONE] finishes', [tok('a'), finish, done], undefined, ['done'], 'a'],
  ['[DONE] with no finish_reason finishes', [tok('a'), done], undefined, ['done'], 'a'],
  ['[DONE] with no trailing newline finishes', [tok('a'), 'data: [DONE]'], undefined, ['done'], 'a'],
  ['finish_reason with no [DONE] finishes (a vendor may omit it)', [tok('a'), finish], undefined, ['done'], 'a'],
  ['a body that just stops is cut off, not finished', [tok('a')], undefined, ['cutoff'], 'a'],
  ['an empty body is cut off', [], undefined, ['cutoff'], ''],
  ['a failed read is cut off', [tok('a')], new TypeError('network error'), ['cutoff'], 'a'],
  ['a failed read after finish_reason finishes (the reply was whole)', [tok('a'), finish], new TypeError('network error'), ['done'], 'a'],
  ['the user stopping it reports nothing', [tok('a')], abort, [], 'a'],
  ['an error frame ends it', [tok('a'), errFrame, done], undefined, ['error:stream_interrupted'], 'a'],
  ['the error frame after a line broken mid-JSON still arrives', [tok('a'), 'data: {"choices":[{"del', '\n\n' + errFrame, done], undefined, ['error:stream_interrupted'], 'a'],
  ['a frame split across reads is put back together', ['data: {"choices":[{"delta":{"con', 'tent":"a"}}]}\n\n', done], undefined, ['done'], 'a'],
]

let failed = 0
for (const [name, parts, fail, end, text] of cases) {
  const got = await run(parts, fail)
  const ok = JSON.stringify(got.end) === JSON.stringify(end) && got.text === text
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}${ok ? '' : ` — got ${JSON.stringify(got)}`}`)
  if (!ok) failed++
}
const tools = await run(['data: {"choices":[],"darkplum":{"type":"tool_call"}}\n\n', done])
if (tools.tools !== 1) { console.log('FAIL tool events reach the chat'); failed++ } else console.log('ok   tool events reach the chat')
// A throw on the page is not a lost connection: the rest of the reply still arrives.
{
  const ends: string[] = []
  let seen = ''
  const original = console.error
  console.error = () => {}
  const body = new ReadableStream<Uint8Array>({ start(c) { c.enqueue(enc.encode(tok('a') + tok('b') + finish + done)); c.close() } })
  await readChatStream(body.getReader(), {
    onToken: (t) => { seen += t; if (t === 'a') throw new Error('render bug') },
    onDone: () => ends.push('done'),
    onErrorFrame: () => ends.push('error'),
    onCutOff: () => ends.push('cutoff'),
  })
  console.error = original
  const ok = seen === 'ab' && JSON.stringify(ends) === '["done"]'
  console.log(`${ok ? 'ok  ' : 'FAIL'} a throwing token handler costs that frame, not the reply${ok ? '' : ` — got ${seen} ${ends}`}`)
  if (!ok) failed++
}
// One end only, even if the handler for it throws.
{
  const ends: string[] = []
  const body = new ReadableStream<Uint8Array>({ start(c) { c.enqueue(enc.encode(done)); c.close() } })
  await readChatStream(body.getReader(), {
    onToken: () => {},
    onDone: () => { ends.push('done'); throw new Error('render bug') },
    onErrorFrame: () => ends.push('error'),
    onCutOff: () => ends.push('cutoff'),
  }).catch(() => {})
  const ok = JSON.stringify(ends) === '["done"]'
  console.log(`${ok ? 'ok  ' : 'FAIL'} a throwing end handler does not end it twice${ok ? '' : ` — got ${ends}`}`)
  if (!ok) failed++
}
const cutOffCases: Array<[string | undefined, boolean, boolean]> = [
  ['stream_interrupted', true, true],
  ['stream_interrupted', false, true],
  ['upstream_error', true, true],
  ['upstream_timeout', true, true],
  ['upstream_error', false, false], // never answered: "did not answer" is true
  ['upstream_timeout', false, false],
  ['provider_rate_limited', true, false], // keeps its own meaning
  [undefined, true, false],
]
for (const [code, hasPartial, want] of cutOffCases) {
  const ok = isCutOffReply(code, hasPartial) === want
  console.log(`${ok ? 'ok  ' : 'FAIL'} isCutOffReply(${code}, ${hasPartial}) = ${want}`)
  if (!ok) failed++
}
if (failed) { console.error(`${failed} failed`); process.exit(1) }
