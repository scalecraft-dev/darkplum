import { ApiError } from '../../lib/api'
import type { LibraryItem, LibraryKind } from '../../types'

/** The server's body cap (migration 026), in bytes. */
export const LIBRARY_BODY_MAX_BYTES = 65536
export const LIBRARY_NAME_MAX = 100
export const LIBRARY_DESC_MAX = 500

export type LibraryTabKey = 'skills' | 'tools' | 'context'

export const TAB_KIND: Record<LibraryTabKey, LibraryKind> = {
  skills: 'skill',
  tools: 'tool',
  context: 'context',
}

/** The noun as it sits in running text: "New skill", "No context matches". */
export const KIND_NOUN: Record<LibraryKind, string> = {
  skill: 'skill',
  tool: 'tool',
  context: 'context',
}

export const KIND_LABEL: Record<LibraryKind, string> = {
  skill: 'Skill',
  tool: 'Tool',
  context: 'Context',
}

/** Plural for list labels and empty states. Context has no plural. */
export const KIND_PLURAL: Record<LibraryKind, string> = {
  skill: 'skills',
  tool: 'tools',
  context: 'context',
}

const encoder = new TextEncoder()

/** Bytes as the server counts them (Go's len on the UTF-8 body). */
export function byteLength(s: string): number {
  return encoder.encode(s).length
}

/** Characters as the server counts names and descriptions (runes). */
export function runeLength(s: string): number {
  return [...s].length
}

export function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const kib = bytes / 1024
  return `${kib < 10 ? kib.toFixed(1) : Math.round(kib)} KB`
}

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' })

/** "3 minutes ago", "yesterday", then a date once it is more than a week old. */
export function formatRelative(iso: string): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ''
  const seconds = Math.round((then - Date.now()) / 1000)
  const abs = Math.abs(seconds)
  if (abs < 45) return 'just now'
  if (abs < 3600) return rtf.format(Math.round(seconds / 60), 'minute')
  if (abs < 86400) return rtf.format(Math.round(seconds / 3600), 'hour')
  if (abs < 7 * 86400) return rtf.format(Math.round(seconds / 86400), 'day')
  return new Date(iso).toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' })
}

export function formatFull(iso: string): string {
  return new Date(iso).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
}

export function ownerName(item: LibraryItem): string {
  return item.owner?.name || item.owner?.email || 'a former member'
}

/** Who can see a row, in the words the list uses. */
export function scopeText(item: LibraryItem): string {
  if (item.mine) return item.visibility === 'org' ? 'Shared by you' : 'Only you'
  return `Shared · by ${ownerName(item)}`
}

/** A tool body as the server stores it. */
export function toolBody(serverUrl: string, allowedTools: string[]): string {
  const cfg: { server_url: string; allowed_tools?: string[] } = { server_url: serverUrl.trim() }
  if (allowedTools.length > 0) cfg.allowed_tools = allowedTools
  return JSON.stringify(cfg)
}

export function parseAllowedTools(raw: string): string[] {
  return raw
    .split(',')
    .map((t) => t.trim())
    .filter(Boolean)
}

export function hostOf(url: string): string | null {
  try {
    return new URL(url).hostname.toLowerCase() || null
  } catch {
    return null
  }
}

/** The api's own matcher (mcp.HostPolicy.Allows), for the admin's warning
 *  about shared tools an allowlist edit would cut off. */
export function hostAllowed(host: string, patterns: string[]): boolean {
  const h = host.toLowerCase().replace(/\.$/, '')
  return patterns.some((raw) => {
    const p = raw.trim().toLowerCase()
    if (!p) return false
    if (p.startsWith('*.')) {
      const suffix = p.slice(1)
      return h.endsWith(suffix) && h.length > suffix.length
    }
    return p === h
  })
}

export function errorText(err: unknown, fallback: string): string {
  if (err instanceof ApiError || err instanceof Error) return err.message || fallback
  return fallback
}

/** The host a tool_host_not_allowed refusal names, when it names one. */
function refusedHost(err: ApiError): string | null {
  const d = err.details as { error?: { host?: unknown } } | undefined
  return typeof d?.error?.host === 'string' ? d.error.host : null
}

/**
 * One sentence per verify failure, by code. The server's prose is not
 * relayed for the host refusal: it points at a screen that does not exist
 * here, and an administrator and everyone else need different next steps.
 */
export function verifyErrorText(err: unknown, opts: { isAdmin: boolean; credentialSet: boolean; serverUrl: string }): string {
  if (!(err instanceof ApiError)) return errorText(err, 'The server could not be verified.')
  const host = refusedHost(err) ?? hostOf(opts.serverUrl) ?? 'This server'
  switch (err.code()) {
    case 'tool_host_not_allowed':
      return opts.isAdmin
        ? `${host} is not on the list of allowed tool servers. Add it under Allowed tool servers at the top of this tab, then verify again.`
        : `${host} is not on your organisation's list of allowed tool servers. Ask an administrator to add it, then verify again.`
    case 'tool_auth_failed':
      return opts.credentialSet
        ? 'The server refused the saved credential. Set a new one below, then verify again.'
        : 'The server asked for a credential. Set one below, then verify again.'
    case 'tool_server_unreachable':
      return `${err.message} Check the address and that the server is running, then verify again.`
    case 'tool_server_error':
      return `${err.message} Check the server's logs, then verify again.`
    default:
      return err.message || 'The server could not be verified.'
  }
}

export function credentialErrorText(err: unknown): string {
  if (err instanceof ApiError && err.code() === 'not_in_cluster') {
    return 'Credentials are stored in the cluster, and this deployment is not running in one. Nothing was saved.'
  }
  return errorText(err, 'The credential could not be saved.')
}
