/**
 * Bedrock data-retention opt-in.
 *
 * Some models on Bedrock (Claude Fable 5 / 5.1 today) are refused by AWS
 * unless the account's Bedrock data-retention mode — set per region — has been
 * raised to `aws_review`. Nothing in darkplum's own configuration is wrong
 * when that happens, and nothing here can fix it: an administrator changes it
 * in AWS. Every sentence the UI says about it lives in this file, so the
 * chat card, the Models row, the picker and the alias editor cannot drift, and
 * check-policy pins them.
 *
 * Pure: no React, no DOM — check-policy imports it directly.
 */

/** The gateway's code, as the 422 body, the SSE error and denial_code carry it. */
export const DATA_RETENTION_CODE = 'model_requires_data_retention'

/** The badge, the activity label and the filter option. One phrase. */
export const AWS_OPT_IN_LABEL = 'Needs AWS opt-in'

/** The mode AWS asks for, used when a row arrives without one. */
const REQUIRED_MODE_FALLBACK = 'aws_review'

interface HasAvailability {
  availability?: {
    state?: string | null
    account_mode?: string | null
    required_mode?: string | null
  } | null
}

/** Only an explicit needs_aws_opt_in counts. 'ok', 'unknown', null and an
 *  absent field (every non-Bedrock row, and an older api) all say nothing. */
export function needsAwsOptIn(m: HasAvailability | null | undefined): boolean {
  return m?.availability?.state === 'needs_aws_opt_in'
}

/** The Models row's statusHint. Names both modes verbatim, because the
 *  administrator is about to type one of them into an AWS API call. */
export function awsOptInStatusHint(m: HasAvailability): string {
  const account = m.availability?.account_mode || 'unknown'
  const required = m.availability?.required_mode || REQUIRED_MODE_FALLBACK
  return (
    `Bedrock refuses this model while your AWS account's data-retention mode is '${account}'; ` +
    `it needs '${required}'. Raising it lets AWS retain and review prompts to models like this ` +
    `one for up to 30 days, inside AWS — the model provider does not receive them. Set it per ` +
    `region with the Bedrock data-retention API; see the runbook.`
  )
}

/** The catalogue summary count. */
export function awsOptInCountText(n: number): string {
  return `${n} ${n === 1 ? 'needs' : 'need'} an AWS opt-in`
}

/** The chat picker's status line for a concrete model that needs it. */
export const AWS_OPT_IN_PICKER_LINE =
  "Needs an AWS opt-in — an administrator has to raise the account's Bedrock data-retention mode"

/** The alias editor's warning under a route (or static alias) whose target
 *  needs it — the same shape as the residency warning beside it. */
export function awsOptInRouteWarning(targetModelId: string): string {
  return `${targetModelId} needs an AWS opt-in before it can answer.`
}

/** The text a native <option> carries, which cannot hold a badge. */
export const AWS_OPT_IN_OPTION_NOTE = 'needs AWS opt-in'

/**
 * The chat failure card for a model_requires_data_retention refusal. The
 * server's own sentence is printed verbatim above the hint by the card; this
 * supplies the headline and, for a routed alias, the one fact the person did
 * not choose — that a route sent it there.
 *
 *   modelName  display name of the model AWS refused
 *   route      the route that picked it, when the gateway named one
 *   alias      the entry the person actually chose
 */
export function dataRetentionFailureCopy(opts: {
  modelName: string
  route?: string | null
  alias?: string | null
}): { title: string; hint: string } {
  const title = `${opts.modelName} needs an AWS opt-in`
  const hint =
    opts.route && opts.alias
      ? `Your message was routed to it via the ${opts.route} route of ${opts.alias}.`
      : opts.route
        ? `Your message was routed to it via the ${opts.route} route.`
        : ''
  return { title, hint }
}
