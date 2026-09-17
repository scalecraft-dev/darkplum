/**
 * Single source of truth for activity event presentation. Both the activity
 * table and the per-user drawer read from here, so a new event type from the
 * API only has to be added once.
 */

export type EventVariant = 'neutral' | 'info' | 'warning' | 'danger' | 'success'

export const EVENT_LABELS: Record<string, { label: string; variant: EventVariant }> = {
  'user.login':            { label: 'Login',            variant: 'neutral'  },
  'user.login_failed':     { label: 'Login Failed',     variant: 'danger'   },
  'user.logout':           { label: 'Logout',           variant: 'neutral'  },
  'user.created':          { label: 'User Created',     variant: 'info'     },
  'user.deleted':          { label: 'User Deleted',     variant: 'info'     },
  'user.role_changed':     { label: 'Role Changed',     variant: 'info'     },
  'user.password_reset':   { label: 'Password Reset',   variant: 'info'     },
  'user.password_changed': { label: 'Password Changed', variant: 'info'     },
  'apikey.created':        { label: 'Key Created',      variant: 'warning'  },
  'apikey.revoked':        { label: 'Key Revoked',      variant: 'warning'  },

  // Model routing. Anything the API sends that is not listed still renders
  // through humanizeEventType(), so an unrecognised type is never blank — it
  // just loses its colour.
  'model_alias.created':    { label: 'Alias Created',          variant: 'info'    },
  'model_alias.updated':    { label: 'Alias Retargeted',       variant: 'info'    },
  'model_alias.deleted':    { label: 'Alias Deleted',          variant: 'info'    },
  'routing_policy.changed': { label: 'Routing Policy Changed', variant: 'warning' },
}

/** Plain-language rendering for detail values that are internal enums. */
const DETAIL_VALUE_LABELS: Record<string, Record<string, string>> = {
  residency: {
    any: 'Any model',
    self_hosted_only: 'Self-hosted only',
  },
}

/** Detail keys whose raw name is clearer than a prettified version. */
const DETAIL_KEY_LABELS: Record<string, string> = {
  target_model_id: 'target',
  residency: 'routing',
}

/** "model_alias.retargeted" -> "Model Alias Retargeted". */
export function humanizeEventType(eventType: string): string {
  const words = eventType.replace(/[._]+/g, ' ').trim()
  if (!words) return 'Unknown Event'
  return words.replace(/\b\w/g, (c) => c.toUpperCase())
}

export function eventLabel(eventType: string): string {
  return EVENT_LABELS[eventType]?.label ?? humanizeEventType(eventType)
}

export function eventVariant(eventType: string): EventVariant {
  return EVENT_LABELS[eventType]?.variant ?? 'neutral'
}

/**
 * Detail values are usually scalars, but never trust that: an object would
 * otherwise render as "[object Object]".
 */
export function formatDetailValue(value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'object') {
    try {
      return JSON.stringify(value)
    } catch {
      return '—'
    }
  }
  return String(value)
}

/** "alias: fast, target: llama-3.1-8b" — or an em dash when there is nothing. */
export function formatEventDetail(detail: Record<string, unknown> | null): string {
  if (!detail) return '—'
  const parts = Object.entries(detail).map(([k, v]) => {
    const key = DETAIL_KEY_LABELS[k] ?? k.replace(/_/g, ' ')
    const raw = formatDetailValue(v)
    const value = DETAIL_VALUE_LABELS[k]?.[raw] ?? raw
    return `${key}: ${value}`
  })
  return parts.length > 0 ? parts.join(', ') : '—'
}
