// A model AWS has retired (end of life) stays in the catalogue, reported by the
// gateway as availability.state 'retired'. Every chat to it fails, so it is
// shown as retired, cannot be turned on, and a chat that reaches it says so.

interface HasAvailability {
  availability?: { state?: string | null } | null
}

/** The code the gateway puts on a chat refused because the model is retired. */
export const MODEL_RETIRED_CODE = 'model_retired'

/** The badge on a Models row. */
export const RETIRED_LABEL = 'Retired by AWS'

/** Only an explicit 'retired' counts; 'ok', 'unknown', null and an absent field
 *  (every non-Bedrock row, and an older api) say nothing. */
export function isRetired(m: HasAvailability | null | undefined): boolean {
  return m?.availability?.state === 'retired'
}

/** The Models row's note. */
export const RETIRED_STATUS_HINT =
  'AWS has retired this model (end of life), so every chat to it fails. It cannot be turned on; enable a current model instead.'

/** The toggle's title while a retired model is off. */
export const RETIRED_TOGGLE_TITLE = 'Retired by AWS: it cannot be enabled'

/** The chat card for a refused retired model. */
export function retiredFailureCopy(isAdmin: boolean): { title: string; hint: string } {
  return {
    title: 'That model has been retired',
    hint: isAdmin
      ? 'Pick another model, or enable a replacement on the Models page.'
      : 'Pick another model from the list at the top of the page.',
  }
}
