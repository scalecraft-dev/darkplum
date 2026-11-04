import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Link, useNavigate } from 'react-router-dom'
import { ChatError, exportConversation, listLibrary, streamChat } from '../lib/api'
import { isCutOffReply } from '../lib/chatStream'
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
  pendingToMessage,
  renameConversation,
  setConversationLibraryItems,
  setConversationModel,
  startConversation,
  toWireMessages,
  truncateFrom,
  userTurn,
  type Conversation,
} from '../lib/conversations'
import { saveBlob } from '../lib/download'
import { pickDefaultModel, promptDestinationClass } from '../lib/modelChoice'
import { greeting } from '../lib/greeting'
import { AttachmentUnavailableError, checkAdd, fromMeta, type AttachmentMeta, type PendingAttachment } from '../lib/attachments'
import { pastedName, prepareFile, releasePreview } from '../lib/attachmentFiles'
import type { BackendClass, ChatMessage, LibraryItem, ToolEvent } from '../types'
import { useAuth } from '../context/AuthContext'
import { useToast } from '../components/Toast'
import { useModels } from '../hooks/useModels'
import { useSidebarSlot } from '../components/SidebarSlot'
import { Button, PlumMark } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import { ConversationList, type ListState } from './ConversationList'
import { MessageList, type ChatFailure } from './MessageList'
import {
  ChatComposer,
  type AttachedChip,
  type ComposerAttachments,
  type ComposerLibrary,
  type ComposerNotice,
  type RejectedAttachment,
} from './ChatComposer'
import { ModelSelector } from './ModelSelector'
import { MODEL_RETIRED_CODE } from '../lib/modelRetired'
import { ModelAccessNotice } from './ModelAccessNotice'
import styles from './ChatPage.module.css'

/* One per kind of work, each a sentence the person can send as-is. */
const STARTER_PROMPTS: { label: string; prompt: string; glyph: React.ReactNode }[] = [
  {
    label: 'Summarize',
    prompt: 'Summarize a document for me',
    glyph: (
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <line x1="4" y1="6" x2="20" y2="6" /><line x1="4" y1="12" x2="16" y2="12" /><line x1="4" y1="18" x2="11" y2="18" />
      </svg>
    ),
  },
  {
    label: 'Query',
    prompt: 'Write a SQL query to find active users',
    glyph: (
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <ellipse cx="12" cy="5" rx="8" ry="3" /><path d="M4 5v14c0 1.7 3.6 3 8 3s8-1.3 8-3V5" /><path d="M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3" />
      </svg>
    ),
  },
  {
    label: 'Debug',
    prompt: 'Explain this error message',
    glyph: (
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <polyline points="16 18 22 12 16 6" /><polyline points="8 6 2 12 8 18" />
      </svg>
    ),
  },
  {
    label: 'Draft',
    prompt: 'Help me draft a professional email',
    glyph: (
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
        <path d="M12 20h9" /><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z" />
      </svg>
    ),
  },
]

/** The model list has five states and each one needs different words. */
type ModelsState = 'loading' | 'error' | 'empty' | 'choose' | 'ready'

/** Refusals that mean the picker is out of date, not that the request was bad.
 *  Each one is grounds for re-reading the list the moment it comes back. */
const STALE_MODEL_CODES = new Set([
  'model_not_found',
  'model_not_enabled',
  // AWS has retired the model; the gateway stops listing it, so re-read.
  'model_retired',
  'model_residency_violation',
  // The list is already filtered by policy when it is read, so this refusal
  // means the policy was narrowed after the picker was filled. Re-reading is
  // the point: the entry should not be on offer any more.
  'model_not_allowed',
])

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
  /** Where the gateway said it ran, and who served it. The only source for
   *  this: a routed alias resolves after the request has left, so nothing on
   *  this side can work it out from the model that was picked. */
  backendClass: BackendClass | null
  provider: string | null
  /** The tool calls the api ran for this reply, in order, and the library
   *  items it attached (from the response header). */
  toolEvents: ToolEvent[]
  libraryUsed: string[]
}

/** How many library items a turn may carry; matches the api. */
const LIBRARY_MAX_ATTACHED = 8

export function ChatPage() {
  const navigate = useNavigate()
  // The conversation list is drawn in the sidebar's Recents section, but it
  // is this page's: its state, loading and edits all stay here.
  const sidebarSlot = useSidebarSlot()
  const { user } = useAuth()
  // Chosen once per visit to the page, not once per render: the line must
  // not change under the person while they type.
  const [openingLine] = useState(() => greeting(user?.name))
  const { toast } = useToast()
  const isAdmin = user?.role === 'admin'
  const {
    models,
    defaultModel,
    isLoading: modelsLoading,
    isRefetching: modelsRefetching,
    error: modelsError,
    policyFiltered,
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
  // Images and files waiting in the composer. Bytes live here, base64, until
  // the turn is saved; the message then carries the stored record instead.
  const [pendingAttachments, setPendingAttachments] = useState<PendingAttachment[]>([])
  const [rejectedAttachments, setRejectedAttachments] = useState<RejectedAttachment[]>([])
  const [attachmentsBusy, setAttachmentsBusy] = useState(false)
  // The tray as of the last commit, for the file loop below: each file is
  // checked against the ones before it without waiting on a render.
  const pendingRef = useRef<PendingAttachment[]>([])
  useEffect(() => {
    pendingRef.current = pendingAttachments
  }, [pendingAttachments])
  // Files being dragged over the chat column: counted, so moving across
  // nested elements does not flicker the overlay.
  const [dragDepth, setDragDepth] = useState(0)
  const pastedCount = useRef(0)
  const abortRef = useRef<AbortController | null>(null)
  /** The library, for the composer's picker and for naming chips and the
   *  "used …" line. Read on first visit and whenever the picker opens. */
  const [libraryItems, setLibraryItems] = useState<LibraryItem[]>([])
  const [libraryLoading, setLibraryLoading] = useState(false)
  const [libraryError, setLibraryError] = useState<string | null>(null)
  /** Attachments chosen before a conversation exists; they move onto the
   *  conversation when it is created. */
  const [draftLibraryIds, setDraftLibraryIds] = useState<string[]>([])
  /** Ids the api refused on the last turn: removed or no longer shared.
   *  The chips stay, muted, until the person removes them. */
  const [missingLibraryIds, setMissingLibraryIds] = useState<string[]>([])
  const modelIds = useMemo(() => models.map((m) => m.id), [models])
  /** Who owns each entry on offer. On an external model that is the vendor,
   *  which is who a failure card names when the gateway's own provider header
   *  did not reach us. */
  const modelOwners = useMemo(
    () => new Map(models.map((m) => [m.id, m.owned_by])),
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
   *  non-null default walking key → user → organisation. Without one,
   *  pickDefaultModel chooses by where the prompt goes rather than by where the
   *  entry happens to fall in the list. Once this has fired, an empty selection
   *  is a deliberate one and stays empty. */
  useEffect(() => {
    if (modelsReady && !model && !autoPickedRef.current && pendingModel === null) {
      autoPickedRef.current = true
      setModel(pickDefaultModel(models, defaultModel))
    }
  }, [modelsReady, model, models, defaultModel, pendingModel])

  /** A conversation reopens on the model it was last used with — but only if
   *  that model is still on offer. When it is gone the picker is left empty for
   *  the person to choose: nobody is moved onto a different model silently. */
  useEffect(() => {
    if (pendingModel === null || !modelsReady) return
    autoPickedRef.current = true
    setModel(modelIds.includes(pendingModel) ? pendingModel : '')
    setPendingModel(null)
  }, [pendingModel, modelsReady, modelIds])

  const patchConv = useCallback((id: string, patch: (conv: Conversation) => Conversation) => {
    setConversations((prev) => prev.map((c) => (c.id === id ? patch(c) : c)))
  }, [])

  const active = conversations.find((c) => c.id === activeId) ?? null
  const attachedIds = active ? active.libraryItemIds : draftLibraryIds
  /** Undefined while the library has not loaded: a reply then says how many
   *  items it used rather than naming ones it cannot know. */
  const libraryNames = useMemo(
    () => (libraryError || (libraryLoading && libraryItems.length === 0) ? undefined : new Map(libraryItems.map((it) => [it.id, it.name]))),
    [libraryItems, libraryError, libraryLoading],
  )

  const refreshLibrary = useCallback(async () => {
    setLibraryLoading(true)
    try {
      setLibraryItems(await listLibrary())
      setLibraryError(null)
    } catch (err) {
      setLibraryError(errorText(err, 'Your library could not be loaded'))
    } finally {
      setLibraryLoading(false)
    }
  }, [])

  useEffect(() => {
    void refreshLibrary()
  }, [refreshLibrary])

  /** Writes a new attachment list to the open conversation (or the draft). */
  const setAttached = useCallback(
    (ids: string[]) => {
      setMissingLibraryIds((prev) => prev.filter((id) => ids.includes(id)))
      if (!active) {
        setDraftLibraryIds(ids)
        return
      }
      const before = active.libraryItemIds
      patchConv(active.id, (c) => ({ ...c, libraryItemIds: ids }))
      if (active.draft) {
        setDraftLibraryIds(ids)
        return
      }
      void setConversationLibraryItems(active.id, ids).catch((err) => {
        // Nothing was saved: the chips go back to what the server holds.
        patchConv(active.id, (c) => ({ ...c, libraryItemIds: before }))
        toast(errorText(err, 'The attachment could not be saved to this conversation'), 'error')
      })
    },
    [active, patchConv, toast],
  )

  const handleToggleAttachment = useCallback(
    (item: LibraryItem) => {
      if (attachedIds.includes(item.id)) {
        setAttached(attachedIds.filter((id) => id !== item.id))
      } else if (attachedIds.length < LIBRARY_MAX_ATTACHED) {
        setAttached([...attachedIds, item.id])
      }
    },
    [attachedIds, setAttached],
  )

  const handleRemoveAttachment = useCallback(
    (id: string) => setAttached(attachedIds.filter((x) => x !== id)),
    [attachedIds, setAttached],
  )

  const chips: AttachedChip[] = useMemo(
    () =>
      attachedIds.map((id) => {
        const item = libraryItems.find((it) => it.id === id)
        return {
          id,
          name: item?.name ?? 'Unavailable item',
          kind: item?.kind ?? 'skill',
          missing: missingLibraryIds.includes(id) || (!item && !libraryLoading && !libraryError),
        }
      }),
    [attachedIds, libraryItems, missingLibraryIds, libraryLoading, libraryError],
  )

  const composerLibrary: ComposerLibrary = useMemo(
    () => ({
      attached: chips,
      onRemove: handleRemoveAttachment,
      onToggle: handleToggleAttachment,
      items: libraryItems,
      loading: libraryLoading,
      error: libraryError,
      onRefresh: () => void refreshLibrary(),
      remaining: Math.max(0, LIBRARY_MAX_ATTACHED - attachedIds.length),
    }),
    [chips, handleRemoveAttachment, handleToggleAttachment, libraryItems, libraryLoading, libraryError, refreshLibrary, attachedIds.length],
  )
  const messages = active?.messages ?? []
  const threadLoading = Boolean(active && loadingId === active.id)

  /** The page is the live copy; the store keeps it so coming back to /chat does
   *  not refetch threads that were read a moment ago. Not while tokens are
   *  arriving: that is a write per token, for a half-finished answer. */
  useEffect(() => {
    if (streaming) return
    for (const conv of conversations) cacheConversation(conv)
  }, [conversations, streaming])

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
  // read the list. Nothing is opened: the page lands on a new chat, and the
  // recents are one click away in the sidebar. A send with nothing open
  // starts a draft (see handleSend).
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
    })()
    return () => {
      cancelled = true
    }
  }, [])

  /** The picker states which model may see this conversation, so the choice is
   *  written to the conversation as well. Without that, a send would still go
   *  out on whatever model the conversation was last opened with. The failure
   *  card stays put: it is the only remaining way back to the refused message. */
  const handleModelChange = useCallback((id: string) => {
    setModel(id)
    autoPickedRef.current = true
    if (!id || !active || active.model === id) return
    patchConv(active.id, (c) => ({ ...c, model: id }))
    // A preference, not content: a conversation that has not been created yet
    // carries it along on the first send, and a failed write is not worth
    // interrupting anyone over.
    if (!active.draft) void setConversationModel(active.id, id).catch(() => {})
  }, [active, patchConv])

  const handleNewChat = useCallback(() => {
    setFailure(null)
    setThreadError(null)
    setMissingLibraryIds([])
    setConversations((prev) => (prev.some((c) => c.draft) ? prev : [createDraft(model, draftLibraryIds), ...prev]))
    setActiveId(DRAFT_ID)
  }, [model, draftLibraryIds])

  const handleSelectConversation = useCallback((id: string) => {
    const conv = conversations.find((c) => c.id === id)
    if (!conv) return
    setActiveId(id)
    setFailure(null)
    setThreadError(null)
    setMissingLibraryIds([])
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

      // A routed alias goes out as the alias on every turn, never as the model
      // that answered last: the gateway classifies the latest message each
      // time, and a thread whose questions change should be routed as they do.
      // A pin stored by an older client is ignored for the same reason.

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
      // What the server recorded for the turn's attachments, once saved: the
      // ids the message carries from then on, in place of the bytes.
      const lastUserAttachments = (full: { role: string; attachments?: AttachmentMeta[] | null }[]) => {
        for (let i = full.length - 1; i >= 0; i--) {
          if (full[i].role === 'user') return full[i].attachments ?? undefined
        }
        return undefined
      }
      const saveUserTurn = (async (): Promise<{
        id: string
        stored: boolean
        diverged: boolean
        attachments?: AttachmentMeta[]
      }> => {
        try {
          if (isDraft) {
            const created = await startConversation({ title, model: picked, messages: unsent, libraryItemIds: conv.libraryItemIds })
            liveId = created.id
            // The attachments now belong to the conversation, not to the
            // next draft.
            setDraftLibraryIds([])
            if (conv.libraryItemIds.length > 0) {
              void setConversationLibraryItems(created.id, conv.libraryItemIds).catch((err) => {
                toast(errorText(err, 'The attachments could not be saved to this conversation'), 'error')
              })
            }
            setConversations((prev) =>
              prev.map((c) =>
                c.id === DRAFT_ID
                  ? { ...created, messages: c.messages, unsavedFrom: null }
                  : c,
              ),
            )
            setActiveId((prev) => (prev === DRAFT_ID ? created.id : prev))
            return {
              id: created.id,
              stored: true,
              diverged: false,
              attachments: lastUserAttachments(created.messages.map((m) => ({ role: m.role, attachments: m.attachments?.map((a) => ({ id: a.id as string, kind: a.kind, media_type: a.mediaType, name: a.name, size_bytes: a.size })) }))),
            }
          }
          if (unsent.length === 0) return { id: liveId, stored: true, diverged: false }
          const lastUnsent = unsent[unsent.length - 1]
          const pendingBytes = (lastUnsent.attachments ?? [])
            .filter((a) => !a.id && a.data)
            .map((a) => ({ kind: a.kind, mediaType: a.mediaType, name: a.name, size: a.size, data: a.data as string }))
          const result = await appendTurns(liveId, [userTurn(lastUnsent.content, pendingBytes)])
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
          return { id: liveId, stored: true, diverged: result.diverged, attachments: lastUserAttachments(result.messages) }
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
        const events: ToolEvent[] = []
        const attempt: Attempt = {
          outcome: 'aborted',
          error: null,
          text: '',
          requestId: null,
          servedModel: null,
          route: null,
          reason: null,
          backendClass: null,
          provider: null,
          toolEvents: events,
          libraryUsed: [],
        }
        const showPartial = () =>
          setConversations((prev) =>
            prev.map((c) =>
              c.id === liveId
                ? {
                    ...c,
                    messages: [
                      ...messagesToSend,
                      { role: 'assistant' as const, content: accum, toolEvents: [...events] },
                    ],
                  }
                : c,
            ),
          )
        let wire
        try {
          wire = toWireMessages(messagesToSend)
        } catch (err) {
          // A turn names an attachment nobody holds any more. Nothing goes out.
          attempt.outcome = 'error'
          attempt.error = err instanceof AttachmentUnavailableError ? err : new Error(String(err))
          abortRef.current = null
          return attempt
        }
        await streamChat(
          wire,
          modelId,
          (token) => {
            accum += token
            showPartial()
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
            attempt.backendClass = meta.backendClass
            attempt.provider = meta.provider
            attempt.libraryUsed = meta.libraryUsed
          },
          {
            libraryItemIds: conv.libraryItemIds,
            onToolEvent: (event) => {
              events.push(event)
              showPartial()
            },
          },
        )
        abortRef.current = null
        attempt.text = accum
        return attempt
      }

      const sendModel = picked
      const result = await streamOnce(sendModel)

      setStreaming(false)

      const target = await saveUserTurn

      // Once stored, the turn carries the records, not the bytes: the
      // thumbnails read back by id, and the next send names the ids so the
      // same megabytes do not cross the wire again.
      if (target.attachments && target.attachments.length > 0) {
        const li = messagesToSend.length - 1
        const lastMsg = messagesToSend[li]
        if (lastMsg.role === 'user' && lastMsg.attachments?.some((a) => !a.id)) {
          for (const a of lastMsg.attachments) releasePreview(a)
          messagesToSend[li] = { ...lastMsg, attachments: target.attachments.map(fromMeta) }
        }
      }

      if (result.outcome === 'error') {
        // The gateway refuses routing failures with a code; keep it so the
        // conversation can explain the refusal instead of "request failed".
        // The failure lives in component state only — it is never part of the
        // conversation, so it is never stored or replayed to a model.
        const err = result.error
        const chatErr = err instanceof ChatError ? err : null
        const code = chatErr?.code
        if (code === 'library_item_not_found' && chatErr?.missing) {
          setMissingLibraryIds(chatErr.missing)
        }
        const hasPartial = Boolean(result.text.trim()) || result.toolEvents.length > 0
        setFailure({
          message: err ? err.message : 'The message was not sent',
          code,
          partial:
            hasPartial && isCutOffReply(code, hasPartial)
              ? { content: result.text, toolEvents: result.toolEvents }
              : null,
          model: sendModel,
          // Who the gateway said served it, falling back to the owner of the
          // entry that was picked. A routed alias is owned by no vendor, and
          // the header is the only thing that knows what it resolved to — so
          // the header goes first and the catalogue is the fallback.
          vendor: result.provider ?? modelOwners.get(sendModel) ?? null,
          param: chatErr?.param ?? null,
          retryAfterSeconds: chatErr?.retryAfterSeconds ?? null,
          refusedModel: chatErr?.refusedModel ?? null,
          route: chatErr?.route ?? null,
          // AWS retired it: it is still in the list, so the list will not say
          // it is gone. The refusal is the evidence.
          modelGone: code === MODEL_RETIRED_CODE ? true : undefined,
        })
        if (code === MODEL_RETIRED_CODE) {
          // Clear the selection now, never pick a replacement: which model sees
          // this conversation is the person's decision.
          setModel((prev) => (prev === sendModel ? '' : prev))
        }
        setConversations((prev) =>
          prev.map((c) => (c.id === target.id ? { ...c, messages: messagesToSend } : c)),
        )
        if (code && STALE_MODEL_CODES.has(code)) void reconcileModels(sendModel)
        return
      }

      // A reply that ended after its tool calls with no words (stopped, or
      // the model said nothing) is kept for what it did, with a line that
      // says so: the history needs text on every assistant turn, and so do
      // the backends on the next send.
      const reply =
        result.text.trim() || result.toolEvents.length === 0
          ? result.text
          : result.outcome === 'aborted'
            ? '(Stopped before replying.)'
            : '(No reply after the tool calls.)'
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
      // The turn went through with these attachments, so none is missing.
      setMissingLibraryIds([])

      let stored = false
      let servedBy: string | null = resolved
      let storedSeq: number | null = null
      let diverged = target.diverged
      if (target.stored) {
        try {
          const appended = await appendTurns(target.id, [
            assistantTurn(reply, resolved, result.requestId, result.toolEvents),
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
          // Same lifetime as the route: the gateway's headers, kept for as
          // long as this copy of the conversation is on screen. Not stored,
          // so a reloaded thread says nothing about where a turn ran rather
          // than claiming something it cannot know.
          backendClass: result.backendClass,
          provider: result.provider,
          toolEvents: result.toolEvents.length > 0 ? result.toolEvents : null,
          libraryUsed: result.libraryUsed.length > 0 ? result.libraryUsed : null,
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
    [streaming, model, modelOwners, reconcileModels, toast],
  )

  const handleSend = useCallback(
    async (text: string, attachments: PendingAttachment[] = []) => {
      // The same two gates sendMessages has, checked here first: the tray is
      // emptied below, and a turn that never goes out must not take the
      // files with it.
      if (streaming || !(model || active?.model)) return
      let conv = active
      if (!conv) {
        conv = createDraft(model, draftLibraryIds)
        const draft = conv
        setConversations((prev) => [draft, ...prev.filter((c) => !c.draft)])
        setActiveId(DRAFT_ID)
      }
      const turn: ChatMessage = { role: 'user', content: text }
      const atts = pendingToMessage(attachments)
      if (atts) turn.attachments = atts
      // The tray empties the moment the turn is on screen; the bytes travel
      // with the message from here on.
      setPendingAttachments([])
      setRejectedAttachments([])
      const messagesToSend: ChatMessage[] = [...conv.messages, turn]
      await sendMessages(conv, messagesToSend)
    },
    [active, model, streaming, sendMessages, draftLibraryIds],
  )

  /** Reads each file, refuses the ones that cannot go, and adds the rest to the
   *  tray. Refusals stay in the tray by name until removed. */
  const addFiles = useCallback(
    async (files: File[]) => {
      if (files.length === 0) return
      setAttachmentsBusy(true)
      try {
        // Every earlier turn's attachments ride along on each send, so they
        // count toward what one more file may add.
        const history = (active?.messages ?? [])
          .filter((m) => m.role === 'user')
          .flatMap((m) => m.attachments ?? [])
          .map((a) => ({ kind: a.kind, size: a.size }))
        for (const raw of files) {
          // The clipboard names every image "image.png".
          const file =
            /^image\.(png|jpe?g|gif|webp)$/i.test(raw.name)
              ? new File([raw], pastedName(++pastedCount.current, raw.type || 'image/png'), { type: raw.type })
              : raw
          const prepared = await prepareFile(file)
          if (!prepared.ok) {
            setRejectedAttachments((prev) => [...prev, { name: prepared.name, error: prepared.error }])
            continue
          }
          const a = prepared.attachment
          const reason = checkAdd(pendingRef.current, a, history)
          if (reason) {
            releasePreview(a)
            setRejectedAttachments((prev) => [...prev, { name: a.name, error: reason }])
            continue
          }
          pendingRef.current = [...pendingRef.current, a]
          setPendingAttachments(pendingRef.current)
        }
      } finally {
        setAttachmentsBusy(false)
      }
    },
    [active],
  )

  const removeAttachment = useCallback((index: number) => {
    setPendingAttachments((prev) => {
      const gone = prev[index]
      if (gone) releasePreview(gone)
      return prev.filter((_, i) => i !== index)
    })
  }, [])

  const dismissRejected = useCallback((index: number) => {
    setRejectedAttachments((prev) => prev.filter((_, i) => i !== index))
  }, [])

  // Where the bytes go, said once, next to the tray: the whole point of the
  // product is that the person knows. In this release only models in the
  // cloud account (Bedrock) read images and files; every other backend
  // refuses them by name, so the control is off there rather than letting a
  // send go out that the gateway will turn away.
  const { residencyLine, attachmentsDisabledReason } = useMemo(() => {
    const m = models.find((x) => x.id === model)
    if (!m) return { residencyLine: undefined, attachmentsDisabledReason: undefined }
    switch (promptDestinationClass(m)) {
      case 'cloud_managed':
        return { residencyLine: 'Attached files stay in your cloud account.', attachmentsDisabledReason: undefined }
      default:
        return {
          residencyLine: undefined,
          attachmentsDisabledReason: 'This model cannot read images or files yet. Pick a model that runs in your cloud account to attach them.',
        }
    }
  }, [models, model])

  const composerAttachments = useMemo<ComposerAttachments>(
    () => ({
      items: pendingAttachments,
      rejected: rejectedAttachments,
      busy: attachmentsBusy,
      onAdd: (files) => void addFiles(files),
      onRemove: removeAttachment,
      onDismissRejected: dismissRejected,
      residency: residencyLine,
      disabledReason: attachmentsDisabledReason,
    }),
    [pendingAttachments, rejectedAttachments, attachmentsBusy, addFiles, removeAttachment, dismissRejected, residencyLine, attachmentsDisabledReason],
  )

  // Files dragged anywhere over the chat column. Only real file drags count:
  // dragging text around the page must not raise the overlay.
  const hasFiles = (e: React.DragEvent) => Array.from(e.dataTransfer.types).includes('Files')
  // No overlay when a drop would be ignored: an invitation the page will not
  // honour is worse than none.
  // A function, not a value: composerDisabled is derived further down.
  const canDrop = () => !streaming && !composerDisabled && !attachmentsDisabledReason
  const onDragEnter = (e: React.DragEvent) => {
    if (!hasFiles(e) || !canDrop()) return
    e.preventDefault()
    setDragDepth((d) => d + 1)
  }
  const onDragLeave = (e: React.DragEvent) => {
    if (!hasFiles(e)) return
    setDragDepth((d) => Math.max(0, d - 1))
  }
  const onDragOver = (e: React.DragEvent) => {
    if (hasFiles(e) && canDrop()) e.preventDefault()
  }
  const onDrop = (e: React.DragEvent) => {
    if (!hasFiles(e)) return
    e.preventDefault()
    setDragDepth(0)
    if (!canDrop()) return
    const files = Array.from(e.dataTransfer.files)
    if (files.length > 0) void addFiles(files)
  }

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
  // Nothing has been said yet and nothing is on its way: the greeting and the
  // composer share the middle of the page, as a blank page should.
  const threadIsNew = !threadLoading && !threadError && messages.length === 0

  const conversationList = (
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
  )

  return (
    <div className={styles.page}>
      {sidebarSlot && createPortal(conversationList, sidebarSlot)}
      <div
        className={`${styles.main} ${threadIsNew ? styles.mainCentered : ''}`}
        onDragEnter={onDragEnter}
        onDragLeave={onDragLeave}
        onDragOver={onDragOver}
        onDrop={onDrop}
      >
        {dragDepth > 0 && (
          <div className={styles.dropOverlay} aria-hidden="true">
            <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round">
              <path d="m21.44 11.05-9.19 9.19a6 6 0 0 1-8.49-8.49l8.57-8.57A4 4 0 1 1 18 8.84l-8.59 8.57a2 2 0 0 1-2.83-2.83l8.49-8.48" />
            </svg>
            <p className={styles.dropTitle}>Drop files to add them</p>
            <p className={styles.dropHint}>Images, PDF, Word, Excel, CSV, HTML, text or Markdown. Images up to 3.75 MB, documents up to 4.5 MB.</p>
          </div>
        )}
        <div className={styles.header}>
          <ModelSelector
            models={models}
            isLoading={modelsLoading}
            error={modelsError}
            value={model}
            onChange={handleModelChange}
          />
          <ModelAccessNotice models={models} suppressError={!policyFiltered} />
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

        {/* The list below is unfiltered when this shows — a model on it can
            still be refused. Not dismissable: the risk lasts as long as the
            policy lookup keeps failing, not as long as someone leaves it up. */}
        {!modelsLoading && !modelsError && !policyFiltered && (
          <div className={styles.policyWarning} role="alert">
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" />
              <line x1="12" y1="9" x2="12" y2="13" />
              <line x1="12" y1="17" x2="12.01" y2="17" />
            </svg>
            <span>
              Model list may be incomplete — the policy service could not be reached. A model shown
              here can still be refused.
            </span>
          </div>
        )}

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
            <h2 className={styles.greeting}>
              <span className={styles.greetingMark} aria-hidden="true">
                <PlumMark size={40} />
              </span>
              {openingLine}
            </h2>

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
            conversationId={active && !active.draft ? active.id : null}
            messages={messages}
            streaming={streaming}
            onRegenerate={handleRegenerate}
            failure={failure}
            isAdmin={isAdmin}
            unsavedFrom={active?.unsavedFrom ?? null}
            libraryNames={libraryNames}
            /* Retrying with nothing selected would repeat the same refused
               request, so the card offers it only once a model is chosen. */
            onRetry={modelsState === 'ready' ? handleRetry : undefined}
          />
        )}

        <ChatComposer
          onSend={(text, atts) => void handleSend(text, atts)}
          onStop={handleStop}
          streaming={streaming}
          disabled={composerDisabled}
          library={composerLibrary}
          attachments={composerAttachments}
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

        {/* On a new chat the starters sit under the composer, where the hands
            already are. They go once the first message is sent. */}
        {threadIsNew && modelsState === 'ready' && (
          <div className={styles.starters}>
            {STARTER_PROMPTS.map((s) => (
              <button
                key={s.prompt}
                type="button"
                className={styles.starterChip}
                onClick={() => handleStarterPrompt(s.prompt)}
                title={s.prompt}
              >
                {s.glyph}
                {s.label}
              </button>
            ))}
          </div>
        )}
        {threadIsNew && modelsState === 'loading' && (
          <div className={styles.starters} aria-hidden="true">
            {STARTER_PROMPTS.map((s) => (
              <Skeleton key={s.prompt} width="96px" height="32px" borderRadius="var(--radius-full)" />
            ))}
          </div>
        )}
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
