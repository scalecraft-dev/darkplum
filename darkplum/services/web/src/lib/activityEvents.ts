/**
 * Single source of truth for activity event presentation. Both the activity
 * table and the per-user drawer read from here, so a new event type from the
 * API only has to be added once.
 */

export type EventVariant = 'neutral' | 'info' | 'warning' | 'danger' | 'success'

/** What one inference row is called and coloured. Both the activity table and
 *  the per-user drawer render inference events, so the decision lives here:
 *  a deliberate refusal (policy, unknown model, bad request) is "Refused"; a
 *  caller pressing Stop is "Cancelled" and not red; anything else at >= 400 is
 *  an "Error" (the backend fell over). Below 400 the row is named for what it
 *  did — a classifier call chose a route, it did not answer anyone. */
export function inferenceEventLabel(e: {
  status_code: number
  denial_code?: string | null
  purpose?: 'inference' | 'routing' | 'routing_dryrun' | null
}): { label: string; variant: EventVariant; cancelled: boolean } {
  const purpose = e.purpose ?? 'inference'
  if (e.status_code < 400) {
    return {
      label: purpose === 'routing' ? 'Classifier' : purpose === 'routing_dryrun' ? 'Route test' : 'Request',
      variant: purpose === 'inference' ? 'success' : 'info',
      cancelled: false,
    }
  }
  if (e.denial_code === 'request_canceled') return { label: 'Cancelled', variant: 'neutral', cancelled: true }
  if (e.denial_code) return { label: 'Refused', variant: 'danger', cancelled: false }
  return { label: 'Error', variant: 'danger', cancelled: false }
}

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

  // Per-principal policy. The organisation event and the per-principal one are
  // named apart: one moves everybody, the other moves one user or one key.
  'routing_policy.org_changed':       { label: 'Org Policy Changed',       variant: 'warning' },
  'settings.system_prompt_changed':   { label: 'System Prompt Changed',    variant: 'warning' },
  'routing_policy.principal_changed': { label: 'Principal Policy Changed', variant: 'warning' },
  // Request parameters (GitHub #3). The switch moves every key at once, so it
  // is amber like the other org-wide changes. A drop is written by the
  // gateway once per key (or webchat user) and parameter, never per request:
  // a notice, not an alarm, so it is info.
  'routing_policy.params_mode_changed': { label: 'Parameter Mode Changed', variant: 'warning' },
  'inference.param_dropped':            { label: 'Parameter Dropped',      variant: 'info'    },

  // Vendor keys. The three that touch the credential are amber, like the API
  // key events above and for the same reason: a reviewer scanning this log for
  // "who changed what we can be billed for" should find them without reading.
  // Installed and rotated are separate rows, not one "set": replacing a key
  // that was working is a different act from installing the first one, and the
  // log is the only place that distinction survives.
  'provider.key_installed': { label: 'Vendor Key Installed', variant: 'warning' },
  'provider.key_rotated':   { label: 'Vendor Key Rotated',   variant: 'warning' },
  'provider.key_removed':   { label: 'Vendor Key Removed',   variant: 'warning' },
  // The billing key is its own credential (an admin key that can list the
  // organisation's workspaces and keys), so it gets its own three rows rather
  // than sharing the inference key's. Amber for the same reason.
  'provider.billing_key_installed': { label: 'Vendor Billing Key Installed', variant: 'warning' },
  'provider.billing_key_rotated':   { label: 'Vendor Billing Key Rotated',   variant: 'warning' },
  'provider.billing_key_removed':   { label: 'Vendor Billing Key Removed',   variant: 'warning' },
  // Not amber: this changes which of a vendor's models are catalogued, which
  // is a configuration change and not a credential one.
  'provider.filter_changed': { label: 'Vendor Models Changed', variant: 'info' },
}

/**
 * Plain-language rendering for detail values that are internal enums.
 *
 * The residency values are the ladder's own words, so an audit row and the
 * policy screen describe the same change identically — an administrator
 * reading "cloud_account_only" in the log and "Your cloud account only" on the
 * card has to work out for themselves that they are the same thing.
 */
const DETAIL_VALUE_LABELS: Record<string, Record<string, string>> = {
  residency: {
    any: 'Any model, including outside your account',
    cloud_account_only: 'Your cloud account only',
    self_hosted_only: 'Self-hosted only',
  },
  residency_before: {
    any: 'Any model, including outside your account',
    cloud_account_only: 'Your cloud account only',
    self_hosted_only: 'Self-hosted only',
  },
  residency_after: {
    any: 'Any model, including outside your account',
    cloud_account_only: 'Your cloud account only',
    self_hosted_only: 'Self-hosted only',
  },
  backend_class: {
    self_hosted: 'Self-hosted',
    cloud_managed: 'Cloud-managed',
    external_api: 'External API',
  },
  vendor: {
    anthropic: 'Anthropic',
    openai: 'OpenAI',
  },
  // routing_policy.params_mode_changed carries {previous, new}; these two
  // keys appear on no other event.
  previous: {
    lenient: 'drop and report',
    strict: 'refuse',
  },
  new: {
    lenient: 'drop and report',
    strict: 'refuse',
  },
  // inference.param_dropped's action.
  action: {
    dropped: 'dropped',
    raised: 'raised',
  },
  filter_source: {
    db: 'Darkplum',
    env: 'the command line',
    none: 'nothing',
  },
}

/** Detail keys whose raw name is clearer than a prettified version. */
const DETAIL_KEY_LABELS: Record<string, string> = {
  target_model_id: 'target',
  residency: 'where requests may run',
  residency_before: 'where requests could run',
  residency_after: 'where requests may now run',
  backend_class: 'ran on',
  allowed_models: 'allowed',
  allowed_models_before: 'allowed before',
  allowed_models_after: 'allowed after',
  default_model: 'default',
  // inference.param_dropped. `param` is a parameter NAME (an unknown key's
  // name is caller-chosen); the value is never recorded.
  param: 'parameter',
  backend: 'backend',
  previous: 'was',
  new: 'now',
  principal_type: 'principal',
  affected_principals: 'principals affected',

  // Vendor keys. `key_last4` is a historical fact about the key that was
  // submitted at that moment — it is computed from the request body and
  // stored, and nothing anywhere reads the installed key to display it. Named
  // "key ending" rather than "key" so a reader is never left wondering whether
  // this row is showing them part of a live credential.
  vendor: 'vendor',
  key_last4: 'key ending',
  model_filter: 'models offered',
  model_filter_before: 'models offered before',
  model_filter_after: 'models offered after',
  filter_source: 'selection set from',
  disabled_models: 'models turned off',
  admitted_count: 'models admitted',
  // The Anthropic workspace a key is scoped to. A name, not a secret.
  workspace_id_before: 'workspace before',
  workspace_id_after: 'workspace after',
}

/** Beyond this, a list of model ids is noise in a table cell. The drawer and
 *  the policy screens carry the full set. */
const MAX_LISTED = 3

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
  // Policy events carry allowlists. A JSON array in a table cell is unreadable;
  // the first few ids and a count is not.
  if (Array.isArray(value)) {
    if (value.length === 0) return 'none'
    const head = value.slice(0, MAX_LISTED).map(String).join(', ')
    return value.length > MAX_LISTED ? `${head} +${value.length - MAX_LISTED} more` : head
  }
  if (typeof value === 'object') {
    try {
      return JSON.stringify(value)
    } catch {
      return '—'
    }
  }
  return String(value)
}

/**
 * Detail keys pinned to the front or the back of the rendered string. The
 * stored order is not the recorded order (JSONB sorts keys by length), and the
 * table cell is truncated, so what identifies a row has to come first and
 * fixed explanatory text last. Every other key keeps its stored order.
 */
const DETAIL_KEYS_FIRST = ['param']
const DETAIL_KEYS_LAST = ['note', 'visible']

function detailKeyRank(k: string): number {
  if (DETAIL_KEYS_FIRST.includes(k)) return -1
  if (DETAIL_KEYS_LAST.includes(k)) return 1
  return 0
}

/** "alias: fast, target: llama-3.1-8b" — or an em dash when there is nothing. */
export function formatEventDetail(detail: Record<string, unknown> | null): string {
  if (!detail) return '—'
  const entries = Object.entries(detail)
    .map((e, i) => ({ e, i }))
    .sort((a, b) => detailKeyRank(a.e[0]) - detailKeyRank(b.e[0]) || a.i - b.i)
    .map(({ e }) => e)
  const parts = entries.map(([k, v]) => {
    const key = DETAIL_KEY_LABELS[k] ?? k.replace(/_/g, ' ')
    const raw = formatDetailValue(v)
    const value = DETAIL_VALUE_LABELS[k]?.[raw] ?? raw
    return `${key}: ${value}`
  })
  return parts.length > 0 ? parts.join(', ') : '—'
}
