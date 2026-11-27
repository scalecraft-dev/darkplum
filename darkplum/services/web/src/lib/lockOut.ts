/**
 * The single `locked_out` entry of a routing-policy 409 that locks out the
 * whole organization rather than named users and keys. The api's
 * orgLockedOutName (services/api/internal/handler/routing.go); check:policy
 * holds the two together. Kept free of imports so that check can load it.
 */
export const ORG_LOCKED_OUT = '(organization)'

/** What an api up to v0.2.0 sends for the same thing. Web and api pods roll
 *  separately, so a new bundle meets an old api during an upgrade. */
const ORG_LOCKED_OUT_LEGACY = '(organisation)'

/** True when a refused save would have locked out everyone. */
export function isOrgLockOut(lockedOut: string[]): boolean {
  return lockedOut.length === 1 && (lockedOut[0] === ORG_LOCKED_OUT || lockedOut[0] === ORG_LOCKED_OUT_LEGACY)
}
