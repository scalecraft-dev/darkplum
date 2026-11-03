import type { ToolEvent } from '../types'

/**
 * Reads a chat completion's SSE body and says how it ended.
 *
 * A body that simply stops is not a finished reply. When a pod holding the stream
 * goes away, what arrived so far looks exactly like a short answer, so it counts as
 * finished only on [DONE] or a finish_reason. Both count because a vendor's stream
 * reaches here as the vendor sent it, and some leave out [DONE].
 */

export const STREAM_CUT_OFF_CODE = 'stream_interrupted'
export const STREAM_CUT_OFF_MESSAGE =
  'The connection was lost before the reply finished.'

/**
 * Whether a failure stopped a reply that was already arriving. The gateway reports a
 * vendor stream that broke with the same codes as a vendor that never answered, and
 * only the page knows whether text had come first, so "did not answer" is not always
 * true. Other codes keep their own meaning.
 */
export function isCutOffReply(code: string | undefined, hasPartial: boolean): boolean {
  if (code === STREAM_CUT_OFF_CODE) return true
  return hasPartial && (code === 'upstream_error' || code === 'upstream_timeout')
}

export interface StreamErrorFrame {
  message: string
  code?: string
  type?: string
  param?: string
  model?: unknown
  route?: unknown
}

export interface ChatStreamHandlers {
  onToken: (token: string) => void
  onToolEvent?: (event: ToolEvent) => void
  onDone: () => void
  onErrorFrame: (error: StreamErrorFrame) => void
  onCutOff: () => void
}

/** Exactly one of onDone, onErrorFrame and onCutOff is called, or none if aborted. */
export async function readChatStream(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  handlers: ChatStreamHandlers,
): Promise<void> {
  const decoder = new TextDecoder()
  let buffer = ''
  let sawFinish = false

  /** A bug on the page drawing one frame costs that frame, not the reply: it is not a
   *  lost connection, and the rest of the stream is still good. */
  const deliver = (handler: () => void) => {
    try {
      handler()
    } catch (err) {
      console.error('chat stream handler failed', err)
    }
  }

  /** True once the line has ended the stream. */
  const handleLine = (line: string): boolean => {
    const trimmed = line.trim()
    if (!trimmed.startsWith('data:')) return false
    const data = trimmed.slice(5).trim()
    if (data === '[DONE]') {
      handlers.onDone()
      return true
    }
    let parsed: {
      choices?: Array<{ delta?: { content?: string }; finish_reason?: string | null }>
      /** The api's own frames while it runs a tool: no choices, one event. An
       *  OpenAI SDK would ignore them; the chat shows them. */
      darkplum?: ToolEvent
      error?: StreamErrorFrame
    }
    try {
      parsed = JSON.parse(data)
    } catch {
      return false // Partial JSON: the line a broken connection left behind.
    }
    if (parsed.error) {
      handlers.onErrorFrame(parsed.error)
      return true
    }
    const event = parsed.darkplum
    if (event && (event.type === 'tool_call' || event.type === 'tool_result')) {
      deliver(() => handlers.onToolEvent?.(event))
      return false
    }
    const choice = parsed.choices?.[0]
    if (choice?.finish_reason) sawFinish = true
    const token = choice?.delta?.content
    if (token) deliver(() => handlers.onToken(token))
    return false
  }

  try {
    while (true) {
      let chunk: ReadableStreamReadResult<Uint8Array>
      try {
        chunk = await reader.read()
      } catch (err) {
        if ((err as Error).name === 'AbortError') return
        // The browser reports a dropped connection as a bare "network error", which
        // says nothing about the reply. How it ended is decided below.
        break
      }
      if (chunk.done) {
        if (handleLine(buffer)) return
        break
      }
      buffer += decoder.decode(chunk.value, { stream: true })
      const lines = buffer.split('\n')
      buffer = lines.pop() ?? ''
      for (const line of lines) {
        if (handleLine(line)) return
      }
    }
  } finally {
    reader.releaseLock()
  }

  if (sawFinish) handlers.onDone()
  else handlers.onCutOff()
}
