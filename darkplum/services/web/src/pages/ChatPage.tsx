import { useCallback, useEffect, useRef, useState } from 'react'
import { streamChat } from '../lib/api'
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
import { useToast } from '../components/Toast'
import { useModels } from '../hooks/useModels'
import { ConversationList } from './ConversationList'
import { MessageList } from './MessageList'
import { ChatComposer } from './ChatComposer'
import { ModelSelector } from './ModelSelector'
import styles from './ChatPage.module.css'

const STARTER_PROMPTS = [
  'Summarize a document for me',
  'Write a SQL query to find active users',
  'Explain this error message',
  'Help me draft a professional email',
]

export function ChatPage() {
  const { toast } = useToast()
  const { models, isLoading: modelsLoading, error: modelsError } = useModels()
  const [conversations, setConversations] = useState<Conversation[]>(() => loadConversations())
  const [activeId, setActiveId] = useState<string | null>(
    () => conversations[0]?.id ?? null,
  )
  const [streaming, setStreaming] = useState(false)
  const [model, setModel] = useState('')
  const abortRef = useRef<AbortController | null>(null)
  const modelIds = models.map((m) => m.id)
  const modelsReady = models.length > 0

  useEffect(() => {
    if (modelsReady && !model) {
      setModel(models[0].id)
    }
  }, [modelsReady, model, models])

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
    const conv = conversations.find((c) => c.id === id)
    if (conv) {
      const validModel = conv.model && modelIds.includes(conv.model)
      setModel(validModel ? conv.model : modelIds[0] ?? '')
    }
  }, [conversations, modelIds])

  const handleDeleteConversation = useCallback((id: string) => {
    deleteConversation(id)
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
          toast(err.message, 'error')
          const rolled = { ...updated, messages: messagesToSend }
          persistConv(rolled)
          setStreaming(false)
          abortRef.current = null
        },
        ctrl.signal,
      )
    },
    [streaming, model, toast, updateConv, persistConv],
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
            onChange={setModel}
          />
        </div>

        {messages.length === 0 ? (
          <div className={styles.emptyState}>
            <h2 className={styles.emptyTitle}>darkplum</h2>
            <p className={styles.emptyTagline}>Private AI, running in your infrastructure.</p>
            <div className={styles.starters}>
              {STARTER_PROMPTS.map((prompt) => (
                <button
                  key={prompt}
                  className={styles.starterBtn}
                  onClick={() => handleStarterPrompt(prompt)}
                  disabled={!modelsReady}
                >
                  {prompt}
                </button>
              ))}
            </div>
          </div>
        ) : (
          <MessageList
            messages={messages}
            streaming={streaming}
            onRegenerate={handleRegenerate}
          />
        )}

        <ChatComposer onSend={handleSend} onStop={handleStop} streaming={streaming} disabled={!modelsReady} />
      </div>
    </div>
  )
}
