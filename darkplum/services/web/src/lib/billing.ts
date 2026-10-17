import type {
  BillingState,
  Decimal,
  ReconciliationGroupBy,
  ReconciliationVendor,
  VendorName,
} from '../types'
import { providerLabel } from './modelRouting.ts'
import { decimalToNumber, formatCost } from './spend.ts'
import { WEB_PORT_FORWARD_URL } from './vendorKeys.ts'

/**
 * Vendor billing keys and the reconciliation they feed.
 *
 * A billing key is NOT the inference key. Anthropic's usage and cost reports
 * need an Admin API key; OpenAI's need an organisation admin key. Both can list
 * the organisation's workspaces/projects and keys, so Darkplum holds them in
 * their own Secret, read only by the api's daily reconciliation job and never
 * by the gateway. Every sentence here keeps that line visible. Bedrock needs
 * no key: it reconciles through AWS Cost Explorer with the api's own role.
 */

/** What each vendor calls the credential, in its console's own words. */
export const BILLING_KEY_NAME: Record<VendorName, string> = {
  anthropic: 'Admin API key',
  openai: 'organisation admin key',
}

/** The one-line explanation under the "Billing key (optional)" heading. */
export function billingKeyExplanation(vendor: VendorName): string {
  const name = providerLabel(vendor)
  return `Reconciles Darkplum's estimates against ${name}'s own usage and cost reports. Needs an ${BILLING_KEY_NAME[vendor]} — a different, broader credential than the inference key above: it can list your organisation's workspaces and keys. It is held by Darkplum's api only, used by one daily job, and never by the gateway.`
}

/** Where to make one, said in the modal beside the field. */
export const BILLING_KEY_WHERE: Record<VendorName, string> = {
  anthropic: 'Create it in the Anthropic Console under Settings > Admin keys. It starts sk-ant-admin01-.',
  openai: 'Create it in the OpenAI platform under Organization settings > Admin keys. A project key will not work.',
}

export const BEDROCK_RECONCILIATION_NOTE =
  "AWS Bedrock needs no key: it reconciles through AWS Cost Explorer with the api's own AWS role, by usage type."

/** The transport refusal, for the billing modal. Shorter than the inference
 *  key's: there is no command-line install for this key. */
export const BILLING_TRANSPORT_HELP = `A billing key can only be sent to Darkplum over HTTPS, or over a port-forward to it. Run the command below, then open ${WEB_PORT_FORWARD_URL} and install the key there.`

export interface BillingStateCopy {
  label: string
  variant: 'success' | 'warning' | 'danger' | 'neutral' | 'info'
  dashed?: boolean
  /** What is true and, where there is something to do, what. */
  detail: string
}

/** Badge and sentence for one vendor's billing state. Bedrock is worded for
 *  the AWS role rather than a key. */
export function billingStateCopy(vendor: ReconciliationVendor, state: BillingState | string): BillingStateCopy {
  const name = providerLabel(vendor)
  const bedrock = vendor === 'bedrock'
  switch (state) {
    case 'ok':
      // "Connected", not "Reconciling": the healthy state is a finished pull,
      // and a word ending in -ing reads as work still under way.
      return {
        label: 'Connected',
        variant: 'success',
        detail: bedrock
          ? "Cost Explorer answered the api's AWS role on the last pull; figures lag up to a day."
          : `${name}'s reports answered the billing key on the last pull.`,
      }
    case 'pulling':
      return { label: 'Pulling', variant: 'info', detail: `Fetching ${name}'s reports now.` }
    case 'not_configured':
      return {
        label: bedrock ? 'Not enabled' : 'No billing key',
        variant: 'neutral',
        dashed: true,
        detail: bedrock
          ? 'No Bedrock usage to reconcile, or Cost Explorer is not enabled for the api role.'
          : `Optional. Without it, ${name} spend is Darkplum's estimate only.`,
      }
    case 'auth_failed':
      return {
        label: 'Key rejected',
        variant: 'danger',
        detail: bedrock
          ? "AWS refused the api's role. It needs ce:GetCostAndUsage."
          : `${name} refused the billing key. It may have been revoked or deleted; replace it.`,
      }
    case 'wrong_key_type':
      return {
        label: 'Wrong kind of key',
        variant: 'danger',
        detail:
          vendor === 'openai'
            ? 'This is a project key. The reports need an organisation admin key created under Organization settings > Admin keys.'
            : 'This is an inference key. The reports need an Admin API key created under Settings > Admin keys.',
      }
    case 'pending_mount':
      return {
        label: 'Key stored, mounting',
        variant: 'warning',
        detail:
          'The billing key is stored and the cluster has not mounted it yet — usually under two minutes. The next pull will use it.',
      }
    case 'unreachable':
      return {
        label: 'Not reachable',
        variant: 'warning',
        detail: `The api could not reach ${name} on its last pull. The next daily pull retries.`,
      }
    default:
      return {
        label: 'Unrecognised state',
        variant: 'neutral',
        dashed: true,
        detail: `The api reported a state this page does not know: ${String(state)}.`,
      }
  }
}

/** A failed verify, worded for the field it is shown under. The api's own
 *  message is used only for codes this bundle does not know. */
export function billingVerifyErrorCopy(
  vendor: VendorName,
  code: string,
  message: string,
  vendorHttpStatus: number | null,
): string {
  const name = providerLabel(vendor)
  switch (code) {
    case 'wrong_key_type':
      return billingStateCopy(vendor, 'wrong_key_type').detail
    case 'auth_failed':
      return `${name} refused this key. Check it was copied whole and has not been revoked.`
    case 'unreachable':
      return `The api could not reach ${name}. Nothing was stored; try again shortly.`
    case 'vendor_error':
      return vendorHttpStatus
        ? `${name} answered with an error (HTTP ${vendorHttpStatus}). Nothing was stored.`
        : `${name} answered with an error. Nothing was stored.`
    default:
      return message
  }
}

/** "Last pulled …", or "Never pulled". */
export function lastPulledText(at: string | null | undefined): string {
  if (!at) return 'Never pulled'
  const d = new Date(at)
  if (Number.isNaN(d.getTime())) return 'Never pulled'
  return `Last pulled ${d.toLocaleString()}`
}

// ---------------------------------------------------------------------------
// Pull now
// ---------------------------------------------------------------------------

/** How long Pull now stays disabled after a press. */
export const PULL_COOLDOWN_MS = 60_000

/** Whole seconds left on the cooldown, 0 when the button is live. A clock
 *  that went backwards does not extend it past the full window. */
export function pullCooldownSeconds(pressedAt: number | null, now: number, windowMs = PULL_COOLDOWN_MS): number {
  if (pressedAt === null) return 0
  const left = pressedAt + windowMs - now
  if (left <= 0) return 0
  return Math.ceil(Math.min(left, windowMs) / 1000)
}

// ---------------------------------------------------------------------------
// The difference column
// ---------------------------------------------------------------------------

export interface DeltaDisplay {
  /** Signed, with a real minus sign: "+$1.23", "−$0.50", "$0.00". */
  text: string
  /** over: the vendor reported more than Darkplum estimated. */
  direction: 'over' | 'under' | 'even'
  /** The non-colour cue beside the number. */
  glyph: '▲' | '▼' | '='
  /** For a screen reader, which hears neither colour nor glyph. */
  spoken: string
}

/**
 * reported − estimated, signed. null in → null out (no figure on one side),
 * which the table shows as a dash. Under half a cent either way is "even":
 * rounding at the vendor is expected and is not a difference worth a colour.
 */
export function formatDelta(cents: Decimal | null | undefined): DeltaDisplay | null {
  const n = decimalToNumber(cents ?? null)
  if (n === null) return null
  if (Math.abs(n) < 0.5) return { text: '$0.00', direction: 'even', glyph: '=', spoken: 'matches the estimate' }
  const abs = formatCost(Math.abs(n)) ?? '$0.00'
  if (n > 0) return { text: `+${abs}`, direction: 'over', glyph: '▲', spoken: `${abs} more than the estimate` }
  return { text: `−${abs}`, direction: 'under', glyph: '▼', spoken: `${abs} less than the estimate` }
}

/** The one sentence on why the two columns will rarely agree exactly. */
export const RECONCILIATION_DELTA_NOTE =
  'Expect differences: estimates use list price while vendors bill your contract rate, both sides round, and vendor reports lag by up to a day. Bedrock is reported by AWS usage type rather than by model.'

export const RECONCILIATION_GROUPS: ReadonlyArray<{ value: ReconciliationGroupBy; label: string; column: string }> = [
  { value: 'day', label: 'Day', column: 'Day (UTC)' },
  { value: 'model', label: 'Model', column: 'Model' },
  { value: 'vendor', label: 'Vendor', column: 'Vendor' },
]

/** The vendors the reconciliation panel always lists, in reading order. */
export const RECONCILIATION_VENDORS: readonly ReconciliationVendor[] = ['anthropic', 'openai', 'bedrock']
