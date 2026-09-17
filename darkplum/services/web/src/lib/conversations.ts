import type { ChatMessage } from '../types'

export interface Conversation {
  id: string
  title: string
  messages: ChatMessage[]
  model: string
  createdAt: string
  updatedAt: string
}

const STORAGE_KEY = 'darkplum:conversations'

function generateId(): string {
  return `conv_${Date.now()}_${Math.random().toString(36).slice(2, 8)}`
}

function generateTitle(messages: ChatMessage[]): string {
  const first = messages.find((m) => m.role === 'user')
  if (!first) return 'New conversation'
  const text = first.content.trim()
  return text.length > 60 ? text.slice(0, 57) + '...' : text
}

const MAX_CONVERSATIONS = 100

let _cache: Conversation[] | null = null

export function loadConversations(): Conversation[] {
  if (_cache) return _cache
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) { _cache = []; return _cache }
    const parsed = JSON.parse(raw) as Conversation[]
    _cache = parsed.map((c) => ({
      ...c,
      model: c.model || '',
      messages: c.messages || [],
    }))
    return _cache
  } catch {
    _cache = []
    return _cache
  }
}

let _persistTimer: ReturnType<typeof setTimeout> | null = null

function persist(conversations: Conversation[]): void {
  _cache = conversations.slice(0, MAX_CONVERSATIONS)
  if (_persistTimer) clearTimeout(_persistTimer)
  _persistTimer = setTimeout(() => {
    try {
      localStorage.setItem(STORAGE_KEY, JSON.stringify(_cache))
    } catch {
      // QuotaExceededError — drop oldest conversations and retry
      const trimmed = (_cache ?? []).slice(0, Math.floor(MAX_CONVERSATIONS / 2))
      try { localStorage.setItem(STORAGE_KEY, JSON.stringify(trimmed)) } catch { /* give up */ }
    }
    _persistTimer = null
  }, 300)
}

export function flushConversations(): void {
  if (_persistTimer && _cache) {
    clearTimeout(_persistTimer)
    localStorage.setItem(STORAGE_KEY, JSON.stringify(_cache))
    _persistTimer = null
  }
}

export function saveConversation(conv: Conversation): void {
  const all = [...loadConversations()]
  const idx = all.findIndex((c) => c.id === conv.id)
  if (idx >= 0) {
    all[idx] = conv
  } else {
    all.unshift(conv)
  }
  persist(all)
}

export function deleteConversation(id: string): void {
  const all = loadConversations().filter((c) => c.id !== id)
  persist(all)
}

export function createConversation(model: string): Conversation {
  return {
    id: generateId(),
    title: 'New conversation',
    messages: [],
    model,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
  }
}

export function updateTitle(conv: Conversation): Conversation {
  return {
    ...conv,
    title: generateTitle(conv.messages),
    updatedAt: new Date().toISOString(),
  }
}
