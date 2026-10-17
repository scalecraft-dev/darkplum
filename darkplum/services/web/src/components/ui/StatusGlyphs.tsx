import styles from './StatusGlyphs.module.css'

/**
 * The marks that carry a state alongside its colour.
 *
 * Every one of these sits next to words that already say the same thing; they
 * exist so the state survives greyscale printing, a colour-blind reader and a
 * glance across a room. They were three private copies on the Models page
 * before vendors arrived and a fourth was needed — one set, one size, one
 * stroke weight, used everywhere a state is shown.
 *
 * All are decorative: `aria-hidden`, never the only thing saying the state.
 */

interface GlyphProps {
  /** Overrides the 11px default where a glyph sits in running text rather than
   *  inside a badge. Keep to the sizes already in use: 11, 13, 14. */
  size?: number
  className?: string
}

function glyphClass(className?: string): string {
  return [styles.glyph, className].filter(Boolean).join(' ')
}

/** Refused by policy. The model is configured; something is saying no. */
export function LockGlyph({ size = 11, className }: GlyphProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className={glyphClass(className)}
    >
      <rect x="3" y="11" width="18" height="11" rx="2" ry="2" />
      <path d="M7 11V7a5 5 0 0 1 10 0v4" />
    </svg>
  )
}

/** Configured, but its backend has not been seen for a few minutes. */
export function UnreachableGlyph({ size = 11, className }: GlyphProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className={glyphClass(className)}
    >
      <circle cx="12" cy="12" r="10" />
      <line x1="4.93" y1="4.93" x2="19.07" y2="19.07" />
    </svg>
  )
}

/**
 * Prompts on this model leave the organisation's cloud account. An arrow
 * crossing out of a boundary — the same shape wherever egress is marked, so an
 * administrator learns it once on the Models page and reads it again without
 * thinking in the picker, the usage log and under a reply.
 */
export function EgressGlyph({ size = 11, className }: GlyphProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className={glyphClass(className)}
    >
      {/* Three sides of a box — the open side is the one being left through. */}
      <path d="M13 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h7" />
      <polyline points="16 8 20 12 16 16" />
      <line x1="20" y1="12" x2="10" y2="12" />
    </svg>
  )
}

/** Nothing has reported this yet. Deliberately not a warning triangle: an
 *  unreported class is a gap in what we know, not a fault we have found. */
export function UnknownGlyph({ size = 11, className }: GlyphProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className={glyphClass(className)}
    >
      <circle cx="12" cy="12" r="10" strokeDasharray="3 3" />
      <path d="M9.2 9.3a3 3 0 0 1 5.8 1c0 2-3 2.5-3 4" />
      <line x1="12" y1="17.5" x2="12.01" y2="17.5" />
    </svg>
  )
}

/**
 * The glyph that belongs to a class, or null when the class needs no mark.
 * Self-hosted and cloud-managed are the unremarkable cases and get nothing:
 * a mark on every row is a mark that means nothing.
 */
export function ClassGlyph({ cls, size, className }: GlyphProps & { cls: string }) {
  if (cls === 'external_api') return <EgressGlyph size={size} className={className} />
  if (cls === 'unknown') return <UnknownGlyph size={size} className={className} />
  return null
}
