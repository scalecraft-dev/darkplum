import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { ChatError, exportConversation, streamChat } from '../lib/api'
import {
  CONVERSATION_PAGE,
  DRAFT_ID,
  NEW_CONVERSATION_TITLE,
  appendTurns,
  assistantTurn,
  cacheConversation,
  createDraft,
  deleteConversation,
  deriveTitle,
  forgetConversation,
  hasLocalConversations,
  importLocalConversations,
  loadConversations,
  openConversation,
  renameConversation,
  setConversationModel,
  setConversationPinnedModel,
  startConversation,
  toWireMessages,
  truncateFrom,
  userTurn,
  type Conversation,
} from '../lib/conversations'
import { saveBlob } from '../lib/download'
import type { ChatMessage } from '../types'
import { useAuth } from '../context/AuthContext'
import { useToast } from '../components/Toast'
import { useModels } from '../hooks/useModels'
import { Button } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import { ConversationList, type ListState } from './ConversationList'
import { MessageList, type ChatFailure } from './MessageList'
import { ChatComposer, type ComposerNotice } from './ChatComposer'
import { ModelSelector } from './ModelSelector'
import { ModelAccessNotice } from './ModelAccessNotice'
import styles from './ChatPage.module.css'

const STARTER_PROMPTS = [
  'Summarize a document for me',
  'Write a SQL query to find active users',
  'Explain this error message',
  'Help me draft a professional email',
]

/** The model list has five states and each one needs different words. */
type ModelsState = 'loading' | 'error' | 'empty' | 'choose' | 'ready'

/** Refusals that mean the picker is out of date, not that the request was bad.
 *  Each one is grounds for re-reading the list the moment it comes back. */
const STALE_MODEL_CODES = new Set([
  'model_not_found',
  'model_not_enabled',
  'model_residency_violation',
  // The list is already filtered by policy when it is read, so this refusal
  // means the policy was narrowed after the picker was filled. Re-reading is
  // the point: the entry should not be on offer any more.
  'model_not_allowed',
])

/** Refusals that say the model this conversation was pinned to has gone. The
 *  alias is still good, so the message is sent again through it and whichever
 *  route the administrator wrote for it answers. */
const PIN_LOST_CODES = new Set(['model_not_found', 'model_not_enabled'])

function errorText(err: unknown, fallback: string): string {
  return err instanceof Error && err.message ? err.message : fallback
}

/** What one pass of the stream produced, including what the gateway said in
 *  its response headers about which model and route answered. */
interface Attempt {
  outcome: 'done' | 'error' | 'aborted'
  error: Error | null
  text: string
  requestId: string | null
  servedModel: string | null
  route: string | null
  reason: string | null
}

export function ChatPage() {
  const navigate = useNavigate()
  const { user } = useAuth()
  const { toast } = useToast()
  const isAdmin = user?.role === 'admin'
  const {
    models,
    defaultModel,
    isLoading: modelsLoading,
    isRefetching: modelsRefetching,
    error: modelsError,
    refetch: refetchModels,
  } = useModels()
  const [conversations, setConversations] = useState<Conversation[]>([])
  const [activeId, setActiveId] = useState<string | null>(null)
  const [listState, setListState] = useState<ListState>('loading')
  const [listError, setListError] = useState<string | null>(null)
  /** How many conversations the rail has asked for. A full page back means
   *  there may be more, so "Load more" asks for one page further. */
  const [listLimit, setListLimit] = useState(CONVERSATION_PAGE)
  const [listComplete, setListComplete] = useState(false)
  const [loadingMore, setLoadingMore] = useState(false)
  const [importFailed, setImportFailed] = useState(false)
  const [importing, setImporting] = useState(false)
  /** The conversation whose bodies are being fetched, if any. */
  const [loadingId, setLoadingId] = useState<string | null>(null)
  const [threadError, setThreadError] = useState<string | null>(null)
  const [exporting, setExporting] = useState(false)
  const [streaming, setStreaming] = useState(false)
  const [model, setModel] = useState('')
  /** A stored model waiting for the picker's list to arrive before it can be
   *  honoured. Restoring it against an empty list would read as "gone". */
  const [pendingModel, setPendingModel] = useState<string | null>(null)
  const [failure, setFailure] = useState<ChatFailure | null>(null)
  const abortRef = useRef<AbortController | null>(null)
  const modelIds = useMemo(() => models.map((m) => m.id), [models])
  /** Entries that are routed aliases: an administrator wrote several routes for
   *  them, so a reply names the route it came through and the conversation is
   *  held to the model that answered first. */
  const routedAliases = useMemo(
    () => new Set(models.filter((m) => m.alias_kind === 'routed').map((m) => m.id)),
    [models],
  )
  const modelsReady = models.length > 0
  /** The default below is a convenience for the first visit only. Once it has
   *  fired, an empty selection is a deliberate one and stays empty. */
  const autoPickedRef = useRef(false)

  const modelsState: ModelsState = modelsReady
    ? model
      ? 'ready'
      : 'choose'
    : modelsLoading
      ? 'loading'
      : modelsError
        ? 'error'
        : 'empty'

  /** The first visit starts on the model the policy nominates — the first
   *  non-null default walking key → user → organisation. Without one, the first
   *  entry on offer. Once this has fired, an empty selection is a deliberate
   *  one and stays empty. */
  useEffect(() => {
    if (modelsReady && !model && !autoPickedRef.current && pendingModel === null) {
      autoPickedRef.current = true
      const preferred = defaultModel && modelIds.includes(defaultModel) ? defaultModel : models[0].id
      setModel(preferred)
    }
  }, [modelsReady, model, models, modelIds, defaultModel, pendingModel])

  /** A conversation reopens on the model it was last used with — but only if
   *  that model is still on offer. When it is gone the picker is left empty for
   *  the person to choose: nobody is moved onto a different model silently. */
  useEffect(() => {
    if (pendingModel === null || !modelsReady) return
    autoPickedRef.current = true
    setModel(modelIds.includes(pendingModel) ? pendingModel : '')
    setPendingModel(null)
  }, [pendingModel, modelsReady, modelIds])

  const active = conversations.find((c) => c.id === activeId) ?? null
  const messages = active?.messages ?? []
  const threadLoading = Boolean(active && loadingId === active.id)

  /** The page is the live copy; the store keeps it so coming back to /chat does
   *  not refetch threads that were read a moment ago. Not while tokens are
   *  arriving: that is a write per token, for a half-finished answer. */
  useEffect(() => {
    if (streaming) return
    for (const conv of conversations) cacheConversation(conv)
  }, [conversations, streaming])

  const patchConv = useCallback((id: string, patch: (conv: Conversation) => Conversation) => {
    setConversations((prev) => prev.map((c) => (c.id === id ? patch(c) : c)))
  }, [])

  const readList = useCallback(async (limit = listLimit) => {
    setListState('loading')
    setListError(null)
    try {
      const list = await loadConversations(limit)
      setConversations((prev) => {
        const draft = prev.find((c) => c.draft)
        return draft ? [draft, ...list] : list
      })
      setListComplete(list.length < limit)
      setListState('ready')
      return list
    } catch (err) {
      setListState('error')
      setListError(errorText(err, 'Your conversations could not be loaded'))
      return null
    }
  }, [listLimit])

  /** One page further back. The whole list is re-read at the larger limit, so
   *  the rail and the in-memory copy stay one thing rather than two halves. */
  const handleLoadMore = useCallback(async () => {
    const next = listLimit + CONVERSATION_PAGE
    setLoadingMore(true)
    try {
      const list = await loadConversations(next)
      setConversations((prev) => {
        const draft = prev.find((c) => c.draft)
        return draft ? [draft, ...list] : list
      })
      setListLimit(next)
      setListComplete(list.length < next)
    } catch (err) {
      toast(errorText(err, 'More conversations could not be loaded'), 'error')
    } finally {
      setLoadingMore(false)
    }
  }, [listLimit, toast])

  const openThread = useCallback(async (id: string) => {
    setLoadingId(id)
    setThreadError(null)
    try {
      const full = await openConversation(id)
      setConversations((prev) => prev.map((c) => (c.id === id ? full : c)))
    } catch (err) {
      setThreadError(errorText(err, 'This conversation could not be loaded'))
    } finally {
      setLoadingId((prev) => (prev === id ? null : prev))
    }
  }, [])

  // First load: move anything still held in this browser to the server, then
  // read the list and open the most recent conversation.
  useEffect(() => {
    let cancelled = false
    void (async () => {
      if (hasLocalConversations()) {
        setImporting(true)
        const result = await importLocalConversations()
        if (cancelled) return
        setImporting(false)
        setImportFailed(result.failed > 0)
      }
      let list: Conversation[] = []
      try {
        list = await loadConversations(CONVERSATION_PAGE)
        if (cancelled) return
        setConversations(list)
        setListComplete(list.length < CONVERSATION_PAGE)
        setListState('ready')
      } catch (err) {
        if (cancelled) return
        setListState('error')
        setListError(errorText(err, 'Your conversations could not be loaded'))
        return
      }
      const first = list[0]
      if (!first) return
      setActiveId(first.id)
      if (first.model) setPendingModel(first.model)
      setLoadingId(first.id)
      try {
        const full = await openConversation(first.id)
        if (cancelled) return
        setConversations((prev) => prev.map((c) => (c.id === full.id ? full : c)))
      } catch (err) {
        if (!cancelled) setThreadError(errorText(err, 'This conversation could not be loaded'))
      } finally {
        if (!cancelled) setLoadingId((prev) => (prev === first.id ? null : prev))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  /** The picker states which model may see this conversation, so the choice is
   *  written to the conversation as well. Without that, a send would still go
   *  out on whatever model the conversation was last opened with. The failure
   *  card stays put: it is the only remaining way back to the refused message. */
  /** Picking a different entry ends any stickiness: the model this conversation
   *  was held to belonged to the entry that has just been replaced. */
  const handleModelChange = useCallback((id: string) => {
    setModel(id)
    autoPickedRef.current = true
    if (!id || !active || active.model === id) return
    const hadPin = active.pinnedModel !== null
    patchConv(active.id, (c) => ({ ...c, model: id, pinnedModel: null }))
    // A preference, not content: a conversation that has not been created yet
    // carries it along on the first send, and a failed write is not worth
    // interrupting anyone over.
    if (!active.draft) {
      void setConversationModel(active.id, id).catch(() => {})
      if (hadPin) void setConversationPinnedModel(active.id, null).catch(() => {})
    }
  }, [active, patchConv])

  const handleNewChat = useCallback(() => {
    setFailure(null)
    setThreadError(null)
    setConversations((prev) => (prev.some((c) => c.draft) ? prev : [createDraft(model), ...prev]))
    setActiveId(DRAFT_ID)
  }, [model])

  const handleSelectConversation = useCallback((id: string) => {
    const conv = conversations.find((c) => c.id === id)
    if (!conv) return
    setActiveId(id)
    setFailure(null)
    setThreadError(null)
    // An untouched new chat is not worth keeping once the person moves on.
    setConversations((prev) =>
      prev.filter((c) => !(c.draft && c.id !== id && c.messages.length === 0)),
    )
    if (conv.model) setPendingModel(conv.model)
    if (!conv.loaded) void openThread(id)
  }, [conversations, openThread])

  const handleDeleteConversation = useCallback(async (id: string) => {
    const conv = conversations.find((c) => c.id === id)
    if (!conv) return
    setConversations((prev) => prev.filter((c) => c.id !== id))
    if (activeId === id) {
      setActiveId(null)
      setFailure(null)
      setThreadError(null)
    }
    if (conv.draft) {
      forgetConversation(id)
      return
    }
    try {
      await deleteConversation(id)
    } catch (err) {
      // Nothing was removed, so the conversation goes back where it was.
      setConversations((prev) =>
        [conv, ...prev].sort((a, b) => b.updatedAt.localeCompare(a.updatedAt)),
      )
      toast(errorText(err, 'The conversation could not be deleted'), 'error')
    }
  }, [conversations, activeId, toast])

  const handleRenameConversation = useCallback(async (id: string, title: string) => {
    const conv = conversations.find((c) => c.id === id)
    const next = title.trim().slice(0, 200)
    if (!conv || !next || next === conv.title) return
    patchConv(id, (c) => ({ ...c, title: next }))
    if (conv.draft) return
    try {
      await renameConversation(id, next)
    } catch (err) {
      patchConv(id, (c) => ({ ...c, title: conv.title }))
      toast(errorText(err, 'The conversation could not be renamed'), 'error')
    }
  }, [conversations, patchConv, toast])

  const handleExport = useCallback(async () => {
    if (!active || active.draft) return
    setExporting(true)
    try {
      const { blob, filename } = await exportConversation(active.id)
      saveBlob(blob, filename)
    } catch (err) {
      toast(errorText(err, 'The conversation could not be downloaded'), 'error')
    } finally {
      setExporting(false)
    }
  }, [active, toast])

  /** A routing refusal is the freshest evidence there is that the picker is
   *  showing a model the gateway no longer serves. Re-read the list, and if the
   *  model has really gone, drop the selection — never substitute another one,
   *  because which model sees this conversation is the person's decision. */
  const reconcileModels = useCallback(async (refusedModel: string) => {
    const latest = await refetchModels()
    // A failed re-read proves nothing; leave the picker as it stands.
    if (!latest || latest.some((m) => m.id === refusedModel)) return
    setFailure((prev) =>
      prev && prev.model === refusedModel ? { ...prev, modelGone: true } : prev,
    )
    setModel((prev) => (prev === refusedModel ? '' : prev))
  }, [refetchModels])

  const sendMessages = useCallback(
    async (conv: Conversation, messagesToSend: ChatMessage[]) => {
      if (streaming) return

      // Only ever the active conversation reaches here, so the picker is this
      // conversation's current choice and outranks the model it was last sent
      // on: what is on screen is what the data goes to.
      const picked = model || conv.model
      if (!picked) return

      // A routed alias resolves to one of the models the administrator wrote
      // routes for. Once one of them has answered, the rest of the conversation
      // goes to that same model rather than being classified again.
      const routed = routedAliases.has(picked)
      const pinned = routed ? conv.pinnedModel : null

      const isDraft = conv.draft === true
      const title =
        isDraft || conv.title === NEW_CONVERSATION_TITLE ? deriveTitle(messagesToSend) : conv.title
      // The id changes under us the moment the server creates the conversation,
      // so every later write looks it up again rather than holding a stale one.
      let liveId = conv.id

      setConversations((prev) =>
        prev.map((c) =>
          c.id === liveId
            ? {
                ...c,
                title,
                // The picker's entry, not the model it resolved to: the alias is
                // what this conversation reopens on.
                model: picked,
                updatedAt: new Date().toISOString(),
                messages: [...messagesToSend, { role: 'assistant' as const, content: '' }],
              }
            : c,
        ),
      )
      setFailure(null)
      setStreaming(true)

      // Turns the server has not taken yet. Normally one — the message just
      // typed. More than one means an earlier save failed: the API takes a turn
      // at a time, so the newest goes and the older one stays flagged.
      const unsent = messagesToSend.slice(conv.syncedCount)
      const gapFrom = unsent.length > 1 ? conv.syncedCount : null

      // Saving runs alongside the request: a reply must never wait on our own
      // database, and a database that is down must not cost anyone their answer.
      const saveUserTurn = (async (): Promise<{
        id: string
        stored: boolean
        diverged: boolean
      }> => {
        try {
          if (isDraft) {
            const created = await startConversation({ title, model: picked, messages: unsent })
            liveId = created.id
            setConversations((prev) =>
              prev.map((c) =>
                c.id === DRAFT_ID
                  ? { ...created, messages: c.messages, unsavedFrom: null }
                  : c,
              ),
            )
            setActiveId((prev) => (prev === DRAFT_ID ? created.id : prev))
            return { id: created.id, stored: true, diverged: false }
          }
          if (unsent.length === 0) return { id: liveId, stored: true, diverged: false }
          const result = await appendTurns(liveId, [userTurn(unsent[unsent.length - 1].content)])
          const last = result.messages[result.messages.length - 1]
          setConversations((prev) =>
            prev.map((c) =>
              c.id === liveId
                ? {
                    ...c,
                    syncedCount: messagesToSend.length,
                    serverSeq: last ? last.seq : c.serverSeq,
                    unsavedFrom: c.unsavedFrom ?? gapFrom,
                  }
                : c,
            ),
          )
          return { id: liveId, stored: true, diverged: result.diverged }
        } catch {
          setConversations((prev) =>
            prev.map((c) =>
              c.id === liveId ? { ...c, unsavedFrom: c.unsavedFrom ?? conv.syncedCount } : c,
            ),
          )
          return { id: liveId, stored: false, diverged: false }
        }
      })()

      // One pass of the stream. Held on an object rather than in locals: the
      // stream reports through callbacks and these are read after it finishes.
      const streamOnce = async (modelId: string): Promise<Attempt> => {
        const ctrl = new AbortController()
        abortRef.current = ctrl
        let accum = ''
        const attempt: Attempt = {
          outcome: 'aborted',
          error: null,
          text: '',
          requestId: null,
          servedModel: null,
          route: null,
          reason: null,
        }
        await streamChat(
          toWireMessages(messagesToSend),
          modelId,
          (token) => {
            accum += token
            setConversations((prev) =>
              prev.map((c) =>
                c.id === liveId
                  ? { ...c, messages: [...messagesToSend, { role: 'assistant' as const, content: accum }] }
                  : c,
              ),
            )
          },
          () => {
            attempt.outcome = 'done'
          },
          (err) => {
            attempt.outcome = 'error'
            attempt.error = err
          },
          ctrl.signal,
          (meta) => {
            attempt.requestId = meta.requestId
            attempt.servedModel = meta.model
            attempt.route = meta.route
            attempt.reason = meta.routerReason
          },
        )
        abortRef.current = null
        attempt.text = accum
        return attempt
      }

      let sendModel = pinned ?? picked
      let result = await streamOnce(sendModel)
      let rerouted = false

      // The model this conversation was held to has gone. The alias has not, so
      // the message goes out once more through it and whichever route the
      // administrator wrote answers instead. Said plainly under the reply.
      const pinRefusedCode =
        pinned !== null && sendModel === pinned && result.outcome === 'error' && !result.text
          ? result.error instanceof ChatError
            ? result.error.code
            : undefined
          : undefined
      if (pinRefusedCode && STALE_MODEL_CODES.has(pinRefusedCode)) {
        // Whatever the refusal was, holding the conversation to a model the
        // gateway will not serve only repeats it. Let the alias choose again.
        setConversations((prev) =>
          prev.map((c) => (c.id === liveId ? { ...c, pinnedModel: null } : c)),
        )
        if (!isDraft) void setConversationPinnedModel(liveId, null).catch(() => {})
        // A model that is gone or off is worth one more try through the alias.
        // A policy refusal is the administrator's decision and stands: it is
        // reported, and the next send classifies afresh.
        if (PIN_LOST_CODES.has(pinRefusedCode)) {
          sendModel = picked
          rerouted = true
          result = await streamOnce(picked)
        }
      }

      setStreaming(false)

      const target = await saveUserTurn

      if (result.outcome === 'error') {
        // The gateway refuses routing failures with a code; keep it so the
        // conversation can explain the refusal instead of "request failed".
        // The failure lives in component state only — it is never part of the
        // conversation, so it is never stored or replayed to a model.
        const err = result.error
        const code = err instanceof ChatError ? err.code : undefined
        setFailure({
          message: err ? err.message : 'The message was not sent',
          code,
          model: sendModel,
        })
        setConversations((prev) =>
          prev.map((c) => (c.id === target.id ? { ...c, messages: messagesToSend } : c)),
        )
        if (code && STALE_MODEL_CODES.has(code)) void reconcileModels(sendModel)
        return
      }

      const reply = result.text
      if (!reply.trim()) {
        // Stopped before a word arrived: there is no turn worth keeping.
        setConversations((prev) =>
          prev.map((c) => (c.id === target.id ? { ...c, messages: messagesToSend } : c)),
        )
        return
      }

      // The gateway names the model that answered in a response header. That is
      // the only thing that knows what a routed alias resolved to — the picker
      // held an alias, and the alias is not a model.
      const resolved = result.servedModel ?? sendModel

      // Hold the rest of the conversation to it, so turn 2 is answered by the
      // same model that answered turn 1.
      if (routed && result.servedModel && result.servedModel !== pinned) {
        const pinTo = result.servedModel
        setConversations((prev) =>
          prev.map((c) => (c.id === target.id ? { ...c, pinnedModel: pinTo } : c)),
        )
        if (target.stored) void setConversationPinnedModel(target.id, pinTo).catch(() => {})
      }

      let stored = false
      let servedBy: string | null = resolved
      let storedSeq: number | null = null
      let diverged = target.diverged
      if (target.stored) {
        try {
          const appended = await appendTurns(target.id, [
            assistantTurn(reply, resolved, result.requestId),
          ])
          const saved = appended.messages[appended.messages.length - 1]
          // The server names the model that actually served the request; it
          // knows, and the picker only ever held an intention.
          if (saved) {
            servedBy = saved.model ?? resolved
            storedSeq = saved.seq
          }
          diverged = diverged || appended.diverged
          stored = true
        } catch {
          stored = false
        }
      }

      const finalMessages: ChatMessage[] = [
        ...messagesToSend,
        {
          role: 'assistant',
          content: reply,
          model: servedBy,
          // History keeps the model per turn, not the route, so these last only
          // as long as this copy of the conversation does.
          route: result.route,
          routerReason: result.reason,
          rerouted,
        },
      ]
      setConversations((prev) =>
        prev.map((c) =>
          c.id === target.id
            ? {
                ...c,
                messages: finalMessages,
                messageCount: storedSeq ?? finalMessages.length,
                // Where the server actually put this turn. Guessing it from the
                // local count would read the next append as another tab's work.
                serverSeq: storedSeq ?? c.serverSeq,
                syncedCount: stored ? finalMessages.length : c.syncedCount,
                unsavedFrom: stored
                  ? c.unsavedFrom
                  : (c.unsavedFrom ?? finalMessages.length - 1),
                updatedAt: new Date().toISOString(),
              }
            : c,
        ),
      )

      // The server put this turn somewhere we did not expect: the same
      // conversation is open elsewhere. Its copy is the real one.
      if (diverged) {
        try {
          const fresh = await openConversation(target.id, { force: true })
          setConversations((prev) => prev.map((c) => (c.id === fresh.id ? fresh : c)))
        } catch {
          // The thread on screen stays as it is until the next read.
        }
      }
    },
    [streaming, model, routedAliases, reconcileModels],
  )

  const handleSend = useCallback(
    async (text: string) => {
      let conv = active
      if (!conv) {
        conv = createDraft(model)
        const draft = conv
        setConversations((prev) => [draft, ...prev.filter((c) => !c.draft)])
        setActiveId(DRAFT_ID)
      }
      const messagesToSend: ChatMessage[] = [...conv.messages, { role: 'user', content: text }]
      await sendMessages(conv, messagesToSend)
    },
    [active, model, sendMessages],
  )

  /** A second answer to the same question replaces the first, on the server as
   *  well as on screen. The stored reply goes first: if it cannot be removed,
   *  regenerating would leave two answers in the history, so nothing is sent. */
  const handleRegenerate = useCallback(async () => {
    if (!active || streaming) return
    const last = active.messages[active.messages.length - 1]
    if (!last || last.role !== 'assistant') return

    const withoutLast = active.messages.slice(0, -1)
    // seq is 1-based and contiguous, so the last turn's seq is the count.
    const fromSeq = active.messages.length
    const kept = fromSeq - 1
    let base = active

    if (active.syncedCount >= fromSeq) {
      try {
        await truncateFrom(active.id, fromSeq)
      } catch (err) {
        patchConv(active.id, (c) => ({
          ...c,
          unsavedFrom: c.unsavedFrom ?? c.messages.length - 1,
        }))
        toast(
          errorText(err, 'The stored reply could not be removed, so nothing was regenerated'),
          'error',
        )
        return
      }
      base = {
        ...active,
        serverSeq: kept,
        messageCount: kept,
        syncedCount: Math.min(active.syncedCount, kept),
      }
      patchConv(active.id, (c) => ({
        ...c,
        serverSeq: kept,
        messageCount: kept,
        syncedCount: Math.min(c.syncedCount, kept),
        // The turn that was flagged as unsaved is gone with the rest.
        unsavedFrom: c.unsavedFrom !== null && c.unsavedFrom >= kept ? null : c.unsavedFrom,
      }))
    }

    await sendMessages(base, withoutLast)
  }, [active, streaming, sendMessages, patchConv, toast])

  // After a failure the last message is the user's own, so resend as-is
  // rather than slicing it off the way regenerate does.
  const handleRetry = useCallback(async () => {
    if (!active || streaming) return
    await sendMessages(active, active.messages)
  }, [active, streaming, sendMessages])

  const handleStop = useCallback(() => {
    // Whatever arrived before the stop is a real answer and is kept, so the
    // send finishes its own bookkeeping rather than being cut short here.
    abortRef.current?.abort()
  }, [])

  const handleStarterPrompt = useCallback((prompt: string) => {
    void handleSend(prompt)
  }, [handleSend])

  useEffect(() => () => {
    abortRef.current?.abort()
  }, [])

  // A conversation whose earlier turns are not on screen must not take a new
  // one: the reply would be missing what came before, and the turn would land
  // after messages nobody can see.
  const threadNotReady = Boolean(active && !active.loaded)
  const composerDisabled = modelsState !== 'ready' || threadNotReady
  const canExport = Boolean(active && !active.draft && active.messageCount > 0)

  return (
    <div className={styles.page}>
      <ConversationList
        conversations={conversations}
        activeId={activeId}
        state={listState}
        error={listError}
        importing={importing}
        importFailed={importFailed}
        onSelect={handleSelectConversation}
        onNew={handleNewChat}
        onDelete={(id) => void handleDeleteConversation(id)}
        onRename={(id, title) => void handleRenameConversation(id, title)}
        onRetry={() => void readList()}
        hasMore={!listComplete}
        loadingMore={loadingMore}
        onLoadMore={() => void handleLoadMore()}
      />
      <div className={styles.main}>
        <div className={styles.header}>
          <ModelSelector
            models={models}
            isLoading={modelsLoading}
            error={modelsError}
            value={model}
            onChange={handleModelChange}
          />
          <ModelAccessNotice models={models} />
          {canExport && (
            <div className={styles.headerActions}>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => void handleExport()}
                disabled={exporting}
                title="Download this conversation as a JSON file"
              >
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
                  <polyline points="7 10 12 15 17 10" />
                  <line x1="12" y1="15" x2="12" y2="3" />
                </svg>
                {exporting ? 'Downloading…' : 'Download'}
              </Button>
            </div>
          )}
        </div>

        {threadLoading ? (
          <div className={styles.threadLoading} role="status" aria-live="polite">
            <span className={styles.srOnly}>Loading this conversation</span>
            <div className={styles.threadSkeleton} aria-hidden="true">
              <Skeleton width="45%" height="44px" borderRadius="var(--radius-lg)" />
              <Skeleton width="72%" height="96px" borderRadius="var(--radius-lg)" />
              <Skeleton width="38%" height="44px" borderRadius="var(--radius-lg)" />
              <Skeleton width="64%" height="72px" borderRadius="var(--radius-lg)" />
            </div>
          </div>
        ) : threadError ? (
          <div className={styles.emptyState}>
            <div className={`${styles.notice} ${styles.noticeError}`} role="alert">
              <h3 className={styles.noticeTitle}>This conversation could not be opened</h3>
              <p className={styles.noticeBody}>
                Its messages are stored on the server and could not be read just now. Nothing
                has been lost.
              </p>
              <p className={styles.noticeDetail}>{threadError}</p>
              <div className={styles.noticeActions}>
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => activeId && void openThread(activeId)}
                >
                  Try again
                </Button>
              </div>
            </div>
          </div>
        ) : messages.length === 0 ? (
          <div className={styles.emptyState}>
            <h2 className={styles.emptyTitle}>darkplum</h2>
            {modelsState !== 'error' && modelsState !== 'empty' && (
              <p className={styles.emptyTagline}>Private AI, running in your infrastructure.</p>
            )}

            {modelsState === 'ready' && (
              <div className={styles.starters}>
                {STARTER_PROMPTS.map((prompt) => (
                  <button
                    key={prompt}
                    className={styles.starterBtn}
                    onClick={() => handleStarterPrompt(prompt)}
                  >
                    {prompt}
                  </button>
                ))}
              </div>
            )}

            {modelsState === 'loading' && (
              <div className={styles.starters} aria-hidden="true">
                {STARTER_PROMPTS.map((prompt) => (
                  <Skeleton key={prompt} width="100%" height="46px" borderRadius="var(--radius-md)" />
                ))}
              </div>
            )}

            {modelsState === 'error' && (
              <div className={`${styles.notice} ${styles.noticeError}`} role="alert">
                <h3 className={styles.noticeTitle}>Can't reach the model gateway</h3>
                <p className={styles.noticeBody}>
                  The list of available models could not be loaded, so nothing can be sent yet.
                </p>
                <p className={styles.noticeDetail}>{modelsError}</p>
                <div className={styles.noticeActions}>
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => void refetchModels()}
                    disabled={modelsRefetching}
                  >
                    {modelsRefetching ? 'Checking…' : 'Try again'}
                  </Button>
                </div>
              </div>
            )}

            {modelsState === 'empty' && (
              <div className={styles.notice} role="status">
                <h3 className={styles.noticeTitle}>No models are available yet</h3>
                {isAdmin ? (
                  <>
                    <p className={styles.noticeBody}>
                      Chat only offers models that are enabled. Turn one on from the Models page
                      and it appears here straight away.
                    </p>
                    <div className={styles.noticeActions}>
                      <Button size="sm" onClick={() => navigate('/admin/models')}>
                        Open Models
                      </Button>
                    </div>
                  </>
                ) : (
                  <p className={styles.noticeBody}>
                    Ask an administrator to enable a model for your organization. Chat starts
                    working as soon as one is on.
                  </p>
                )}
              </div>
            )}
          </div>
        ) : (
          <MessageList
            messages={messages}
            streaming={streaming}
            onRegenerate={handleRegenerate}
            failure={failure}
            isAdmin={isAdmin}
            unsavedFrom={active?.unsavedFrom ?? null}
            /* Retrying with nothing selected would repeat the same refused
               request, so the card offers it only once a model is chosen. */
            onRetry={modelsState === 'ready' ? handleRetry : undefined}
          />
        )}

        <ChatComposer
          onSend={handleSend}
          onStop={handleStop}
          streaming={streaming}
          disabled={composerDisabled}
          /* The empty-state card has its own Try again; offer one here only
             when that card is not on screen. */
          notice={
            threadLoading
              ? { text: 'Opening this conversation…' }
              : threadNotReady
              ? { text: 'This conversation has not loaded, so a reply would be missing what came before it.' }
              : composerNotice(
                  modelsState,
                  isAdmin,
                  messages.length > 0 ? () => void refetchModels() : undefined,
                  modelsRefetching,
                )
          }
        />
      </div>
    </div>
  )
}

/** Why the composer is off, in the words that matter to the person reading.
 *  With a conversation already open there is no empty-state card, so this line
 *  is the only explanation on screen — it carries the way out with it. */
function composerNotice(
  state: ModelsState,
  isAdmin: boolean,
  onRetryModels?: () => void,
  isRefetching = false,
): ComposerNotice | undefined {
  switch (state) {
    case 'ready':
      return undefined
    case 'loading':
      return { text: 'Loading the list of available models…' }
    case 'choose':
      return {
        text: 'The model this conversation was using is no longer available. Choose another one above to continue.',
      }
    case 'error':
      return {
        text: 'Chat is paused until the model gateway responds.',
        action: onRetryModels
          ? { label: isRefetching ? 'Checking…' : 'Try again', onClick: onRetryModels }
          : undefined,
      }
    case 'empty':
      return isAdmin
        ? {
            text: 'No models are enabled. Turn one on in Models to start chatting.',
            content: (
              <>
                No models are enabled. Turn one on in <Link to="/admin/models">Models</Link> to
                start chatting.
              </>
            ),
          }
        : { text: 'No models are available. Ask an administrator to enable one.' }
  }
}
