import { useCallback, useEffect, useRef, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { ChatError, streamChat } from '../lib/api'
import {
  createConversation,
  deleteConversation,
  flushConversations,
  loadConversations,
  saveConversation,
  updateTitle,
  type Conversation,
} from '../lib/conversations'
import type { ChatMessage } from '../types'
import { useAuth } from '../context/AuthContext'
import { useModels } from '../hooks/useModels'
import { Button } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import { ConversationList } from './ConversationList'
import { MessageList, type ChatFailure } from './MessageList'
import { ChatComposer, type ComposerNotice } from './ChatComposer'
import { ModelSelector } from './ModelSelector'
import styles from './ChatPage.module.css'

const STARTER_PROMPTS = [
  'Summarize a document for me',
  'Write a SQL query to find active users',
  'Explain this error message',
  'Help me draft a professional email',
]

/** The model list has four states and each one needs different words. */
type ModelsState = 'loading' | 'error' | 'empty' | 'ready'

export function ChatPage() {
  const navigate = useNavigate()
  const { user } = useAuth()
  const isAdmin = user?.role === 'admin'
  const { models, isLoading: modelsLoading, error: modelsError, refetch: refetchModels } = useModels()
  const [conversations, setConversations] = useState<Conversation[]>(() => loadConversations())
  const [activeId, setActiveId] = useState<string | null>(
    () => conversations[0]?.id ?? null,
  )
  const [streaming, setStreaming] = useState(false)
  const [model, setModel] = useState('')
  const [failure, setFailure] = useState<ChatFailure | null>(null)
  const abortRef = useRef<AbortController | null>(null)
  const modelIds = models.map((m) => m.id)
  const modelsReady = models.length > 0

  const modelsState: ModelsState = modelsReady
    ? 'ready'
    : modelsLoading
      ? 'loading'
      : modelsError
        ? 'error'
        : 'empty'

  useEffect(() => {
    if (modelsReady && !model) {
      setModel(models[0].id)
    }
  }, [modelsReady, model, models])

  const handleModelChange = useCallback((id: string) => {
    setModel(id)
    setFailure(null)
  }, [])

  const active = conversations.find((c) => c.id === activeId) ?? null
  const messages = active?.messages ?? []

  const updateConv = useCallback((conv: Conversation) => {
    setConversations((prev) => {
      const idx = prev.findIndex((c) => c.id === conv.id)
      if (idx >= 0) {
        const next = [...prev]
        next[idx] = conv
        return next
      }
      return [conv, ...prev]
    })
  }, [])

  const persistConv = useCallback((conv: Conversation) => {
    saveConversation(conv)
    updateConv(conv)
  }, [updateConv])

  const handleNewChat = useCallback(() => {
    const conv = createConversation(model)
    persistConv(conv)
    setActiveId(conv.id)
  }, [model, persistConv])

  const handleSelectConversation = useCallback((id: string) => {
    setActiveId(id)
    setFailure(null)
    const conv = conversations.find((c) => c.id === id)
    if (conv) {
      const validModel = conv.model && modelIds.includes(conv.model)
      setModel(validModel ? conv.model : modelIds[0] ?? '')
    }
  }, [conversations, modelIds])

  const handleDeleteConversation = useCallback((id: string) => {
    deleteConversation(id)
    setFailure(null)
    setConversations((prev) => {
      const next = prev.filter((c) => c.id !== id)
      if (activeId === id) {
        setActiveId(next[0]?.id ?? null)
      }
      return next
    })
  }, [activeId])

  const sendMessages = useCallback(
    async (conv: Conversation, messagesToSend: ChatMessage[]) => {
      if (streaming) return

      const sendModel = conv.model || model
      const updatedMessages: ChatMessage[] = [...messagesToSend, { role: 'assistant', content: '' }]
      let updated = { ...conv, messages: updatedMessages, model: sendModel, updatedAt: new Date().toISOString() }

      if (messagesToSend.filter((m) => m.role === 'user').length === 1) {
        updated = updateTitle(updated)
      }

      updateConv(updated)
      setFailure(null)
      setStreaming(true)

      const ctrl = new AbortController()
      abortRef.current = ctrl

      let accum = ''
      await streamChat(
        messagesToSend,
        sendModel,
        (token) => {
          accum += token
          const withToken = { ...updated, messages: [...messagesToSend, { role: 'assistant' as const, content: accum }] }
          updateConv(withToken)
        },
        () => {
          const final = { ...updated, messages: [...messagesToSend, { role: 'assistant' as const, content: accum }] }
          persistConv(final)
          setStreaming(false)
          abortRef.current = null
        },
        (err) => {
          // The gateway refuses routing failures with a code; keep it so the
          // conversation can explain the refusal instead of "request failed".
          setFailure({
            message: err.message,
            code: err instanceof ChatError ? err.code : undefined,
            model: sendModel,
          })
          const rolled = { ...updated, messages: messagesToSend }
          persistConv(rolled)
          setStreaming(false)
          abortRef.current = null
        },
        ctrl.signal,
      )
    },
    [streaming, model, updateConv, persistConv],
  )

  const handleSend = useCallback(
    async (text: string) => {
      let conv = active
      if (!conv) {
        conv = createConversation(model)
        persistConv(conv)
        setActiveId(conv.id)
      }
      const userMsg: ChatMessage = { role: 'user', content: text }
      const messagesToSend = [...conv.messages, userMsg]
      await sendMessages(conv, messagesToSend)
    },
    [active, model, sendMessages, persistConv],
  )

  const handleRegenerate = useCallback(async () => {
    if (!active || streaming) return
    const withoutLast = active.messages.slice(0, -1)
    await sendMessages(active, withoutLast)
  }, [active, streaming, sendMessages])

  // After a failure the last message is the user's own, so resend as-is
  // rather than slicing it off the way regenerate does.
  const handleRetry = useCallback(async () => {
    if (!active || streaming) return
    await sendMessages(active, active.messages)
  }, [active, streaming, sendMessages])

  const handleStop = useCallback(() => {
    abortRef.current?.abort()
    if (active) {
      persistConv({ ...active, updatedAt: new Date().toISOString() })
    }
    setStreaming(false)
  }, [active, persistConv])

  const handleStarterPrompt = useCallback((prompt: string) => {
    void handleSend(prompt)
  }, [handleSend])

  useEffect(() => () => {
    abortRef.current?.abort()
    flushConversations()
  }, [])

  return (
    <div className={styles.page}>
      <ConversationList
        conversations={conversations}
        activeId={activeId}
        onSelect={handleSelectConversation}
        onNew={handleNewChat}
        onDelete={handleDeleteConversation}
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
        </div>

        {messages.length === 0 ? (
          <div className={styles.emptyState}>
            <h2 className={styles.emptyTitle}>darkplum</h2>
            {(modelsState === 'ready' || modelsState === 'loading') && (
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
                  <Button variant="secondary" size="sm" onClick={() => void refetchModels()}>
                    Try again
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
            onRetry={modelsReady ? handleRetry : undefined}
          />
        )}

        <ChatComposer
          onSend={handleSend}
          onStop={handleStop}
          streaming={streaming}
          disabled={!modelsReady}
          /* The empty-state card has its own Try again; offer one here only
             when that card is not on screen. */
          notice={composerNotice(
            modelsState,
            isAdmin,
            messages.length > 0 ? () => void refetchModels() : undefined,
          )}
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
): ComposerNotice | undefined {
  switch (state) {
    case 'ready':
      return undefined
    case 'loading':
      return { text: 'Loading the list of available models…' }
    case 'error':
      return {
        text: 'Chat is paused until the model gateway responds.',
        action: onRetryModels ? { label: 'Try again', onClick: onRetryModels } : undefined,
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
