import type {
  ChatMessage,
  ConversationFull,
  ConversationMessage,
  ConversationMessageInput,
  ConversationSummary,
} from '../types'
import {
  appendConversationMessages,
  createConversation,
  deleteConversation as deleteConversationRequest,
  getConversation,
  listConversations,
  truncateConversationMessages,
  updateConversation,
} from './api'

/**
 * Conversation history lives on the server, under the signed-in person's own
 * account. This module is the only place that knows that: it holds an
 * in-memory copy so switching between conversations is instant, and it knows
 * how much of each conversation the server has actually accepted.
 */
export interface Conversation {
  id: string
  title: string
  /** The model the conversation was last sent on. '' when none was chosen. For
   *  a routed alias this is the alias, which is what the picker shows. */
  model: string
  /** Empty until the conversation is opened — the list endpoint sends no bodies. */
  messages: ChatMessage[]
  createdAt: string
  updatedAt: string
  /** From the summary, so the list can show a thread's length before it loads. */
  messageCount: number
  /** True once the bodies have been fetched. */
  loaded: boolean
  /** How many of the messages above are known to be on the server, counted from
   *  the start. Anything past this has not been stored. */
  syncedCount: number
  /** The highest seq the server has assigned. A reply that skips ahead of this
   *  means another tab wrote to the same conversation. */
  serverSeq: number
  /** Index of the first turn a save failed on, or null while everything is
   *  stored. Drives the "not saved" notice in the thread. */
  unsavedFrom: number | null
  /** A conversation that exists only in this tab. Nothing is created on the
   *  server until the first message is sent. */
  draft?: boolean
}

/** The one conversation that has not been created yet. */
export const DRAFT_ID = 'draft:new'

export const NEW_CONVERSATION_TITLE = 'New conversation'

/** Pre-migration history. Read once, moved to the server, then removed. */
const LEGACY_STORAGE_KEY = 'darkplum:conversations'

/** Matches the column check on conversation_messages.content. */
const MAX_TURN_BYTES = 262_144

const TITLE_MAX = 60

/** The server derives titles the same way; doing it here too means the list
 *  reads correctly the moment a first message is sent, not a round-trip later. */
export function deriveTitle(messages: ChatMessage[]): string {
  const first = messages.find((m) => m.role === 'user')
  if (!first) return NEW_CONVERSATION_TITLE
  const text = first.content.trim().replace(/\s+/g, ' ')
  if (!text) return NEW_CONVERSATION_TITLE
  return text.length > TITLE_MAX ? `${text.slice(0, TITLE_MAX - 3)}...` : text
}

// --- in-memory cache -------------------------------------------------------
// Survives leaving /chat and coming back, so a return trip does not refetch
// every thread the person already read.

const cache = new Map<string, Conversation>()
let order: string[] = []

function fromSummary(s: ConversationSummary): Conversation {
  const existing = cache.get(s.id)
  // A cached thread is still good while the server's count agrees with it;
  // anything else (another tab, another machine) is refetched on open.
  const stillFresh = existing?.loaded === true && existing.messageCount === s.message_count
  return {
    id: s.id,
    title: s.title,
    model: s.model ?? '',
    messages: stillFresh ? existing.messages : [],
    createdAt: s.created_at,
    updatedAt: s.updated_at,
    messageCount: s.message_count,
    loaded: stillFresh,
    syncedCount: stillFresh ? existing.syncedCount : s.message_count,
    serverSeq: s.message_count,
    unsavedFrom: stillFresh ? existing.unsavedFrom : null,
  }
}

function fromFull(full: ConversationFull): Conversation {
  const messages: ChatMessage[] = full.messages.map((m) => ({
    role: m.role,
    content: m.content,
    model: m.model,
  }))
  const lastSeq = full.messages.length > 0 ? full.messages[full.messages.length - 1].seq : 0
  return {
    id: full.id,
    title: full.title,
    model: full.model ?? '',
    messages,
    createdAt: full.created_at,
    updatedAt: full.updated_at,
    messageCount: full.message_count,
    loaded: true,
    syncedCount: messages.length,
    serverSeq: lastSeq,
    unsavedFrom: null,
  }
}

function put(conv: Conversation): Conversation {
  cache.set(conv.id, conv)
  if (!order.includes(conv.id)) order = [conv.id, ...order]
  return conv
}

/** Everything held in memory, newest first. Empty before the first load. */
export function cachedConversations(): Conversation[] {
  return order.map((id) => cache.get(id)).filter((c): c is Conversation => Boolean(c))
}

/** How many conversations the rail asks for at a time. "Load more" asks for
 *  one page more, rather than a second page: the endpoint takes a limit, and a
 *  growing limit keeps the in-memory copy in one piece. */
export const CONVERSATION_PAGE = 30

/** Reads the list of conversations. Summaries only — bodies arrive on open. */
export async function loadConversations(limit = CONVERSATION_PAGE): Promise<Conversation[]> {
  const summaries = await listConversations({ limit })
  const next = summaries.map(fromSummary)
  cache.clear()
  order = []
  for (const conv of next) {
    cache.set(conv.id, conv)
    order.push(conv.id)
  }
  return next
}

/** Reads one conversation in full. Cached threads are returned as they stand
 *  unless the caller knows they are behind. */
export async function openConversation(
  id: string,
  opts: { force?: boolean } = {},
): Promise<Conversation> {
  const cached = cache.get(id)
  if (!opts.force && cached?.loaded) return cached
  return put(fromFull(await getConversation(id)))
}

/** A conversation that exists only here, until the first message is sent. */
export function createDraft(model: string): Conversation {
  const now = new Date().toISOString()
  return {
    id: DRAFT_ID,
    title: NEW_CONVERSATION_TITLE,
    model,
    messages: [],
    createdAt: now,
    updatedAt: now,
    messageCount: 0,
    loaded: true,
    syncedCount: 0,
    serverSeq: 0,
    unsavedFrom: null,
    draft: true,
  }
}

/** Creates the conversation on the first send, carrying the turns that prompted it. */
export async function startConversation(input: {
  title: string
  model: string
  messages: ChatMessage[]
}): Promise<Conversation> {
  const full = await createConversation({
    title: input.title,
    model: input.model,
    messages: input.messages.map(toInput),
  })
  return put(fromFull(full))
}

export interface AppendResult {
  messages: ConversationMessage[]
  /** The server assigned a seq past the one we expected: somewhere else — another
   *  tab, another window — wrote to this conversation. The caller refetches. */
  diverged: boolean
}

/** Appends turns and records where the server put them. */
export async function appendTurns(
  id: string,
  messages: ConversationMessageInput[],
): Promise<AppendResult> {
  const cached = cache.get(id)
  const expected = (cached?.serverSeq ?? 0) + 1
  const res = await appendConversationMessages(id, messages)
  const appended = res.messages
  const first = appended[0]
  const last = appended[appended.length - 1]
  if (cached && last) {
    cache.set(id, { ...cached, serverSeq: last.seq, messageCount: last.seq })
  }
  return { messages: appended, diverged: Boolean(first) && first.seq !== expected }
}

/**
 * Drops the turn at fromSeq and everything after it. Regenerating an answer
 * replaces the one it was asked to better; without this the old reply would
 * still be the stored one and both would come back on the next read.
 */
export async function truncateFrom(id: string, fromSeq: number): Promise<void> {
  await truncateConversationMessages(id, fromSeq)
  const cached = cache.get(id)
  if (!cached) return
  const kept = fromSeq - 1
  cache.set(id, {
    ...cached,
    serverSeq: kept,
    messageCount: kept,
    syncedCount: Math.min(cached.syncedCount, kept),
  })
}

/** Records, in the cache, what the page now believes about a conversation. */
export function cacheConversation(conv: Conversation): void {
  if (conv.draft) return
  put(conv)
}

export async function renameConversation(id: string, title: string): Promise<void> {
  await updateConversation(id, { title })
  const cached = cache.get(id)
  if (cached) cache.set(id, { ...cached, title })
}

/** Records the model this conversation should reopen on. */
export async function setConversationModel(id: string, model: string): Promise<void> {
  await updateConversation(id, { model })
  const cached = cache.get(id)
  if (cached) cache.set(id, { ...cached, model })
}

export async function deleteConversation(id: string): Promise<void> {
  await deleteConversationRequest(id)
  forgetConversation(id)
}

export function forgetConversation(id: string): void {
  cache.delete(id)
  order = order.filter((existing) => existing !== id)
}

/** Strips the display-only fields before a turn goes back to the gateway. */
export function toWireMessages(messages: ChatMessage[]): ChatMessage[] {
  return messages.map((m) => ({ role: m.role, content: m.content }))
}

function toInput(m: ChatMessage): ConversationMessageInput {
  const input: ConversationMessageInput = { role: m.role, content: fit(m.content) }
  if (m.role === 'assistant' && m.model) input.model = m.model
  return input
}

export function userTurn(content: string): ConversationMessageInput {
  return { role: 'user', content: fit(content) }
}

/** The model and request id are what the server fills in from its own audit
 *  record; they are sent for servers that do not, and what comes back wins. */
export function assistantTurn(
  content: string,
  model: string,
  requestId: string | null,
): ConversationMessageInput {
  const input: ConversationMessageInput = { role: 'assistant', content: fit(content) }
  if (model) input.model = model
  if (requestId) input.request_id = requestId
  return input
}

/** Keeps one turn inside the column's limit. Long replies are cut rather than
 *  rejected, so a single outsized turn cannot cost someone the conversation. */
function fit(content: string): string {
  const encoder = new TextEncoder()
  if (encoder.encode(content).length <= MAX_TURN_BYTES) return content
  let text = content
  while (text.length > 0 && encoder.encode(text).length > MAX_TURN_BYTES) {
    text = text.slice(0, Math.floor(text.length * 0.9))
  }
  return text
}

// --- one-time move off this device ----------------------------------------

interface LegacyConversation {
  id: string
  title?: string
  model?: string
  messages?: ChatMessage[]
  createdAt?: string
  updatedAt?: string
}

function readLegacy(): LegacyConversation[] {
  try {
    const raw = localStorage.getItem(LEGACY_STORAGE_KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw) as LegacyConversation[]
    if (!Array.isArray(parsed)) return []
    return parsed.filter((c) => c && typeof c.id === 'string')
  } catch {
    return []
  }
}

/** True while history from before the move is still sitting in this browser. */
export function hasLocalConversations(): boolean {
  return readLegacy().some((c) => (c.messages ?? []).length > 0)
}

export interface ImportResult {
  total: number
  imported: number
  failed: number
}

/**
 * Moves history off this device, oldest first, one at a time. Each conversation
 * carries its local id as import_key, so a run that dies halfway can be repeated
 * without duplicating anything. The local copy is only dropped once every
 * conversation has been accepted — a half-moved history stays where it is.
 */
export async function importLocalConversations(): Promise<ImportResult> {
  const legacy = readLegacy()
    .filter((c) => (c.messages ?? []).length > 0)
    .sort((a, b) => (a.createdAt ?? '').localeCompare(b.createdAt ?? ''))

  let imported = 0
  let failed = 0

  for (const conv of legacy) {
    const messages = (conv.messages ?? [])
      .filter((m) => (m.role === 'user' || m.role === 'assistant') && m.content.trim() !== '')
      .map(toInput)
    if (messages.length === 0) continue
    try {
      await createConversation({
        title: (conv.title ?? '').trim().slice(0, 200) || deriveTitle(conv.messages ?? []),
        model: conv.model ?? '',
        import_key: conv.id,
        messages,
      })
      imported += 1
    } catch {
      failed += 1
    }
  }

  if (failed === 0) {
    try {
      localStorage.removeItem(LEGACY_STORAGE_KEY)
    } catch {
      // A browser that refuses to forget is not worth blocking chat over; the
      // import_key makes a repeat run harmless.
    }
  }

  return { total: legacy.length, imported, failed }
}
