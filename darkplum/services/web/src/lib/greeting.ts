/**
 * The line a new chat opens with.
 *
 * Time of day sets the register, the person's first name makes it theirs, and
 * a small set of variants per register keeps the screen from greeting the
 * same way every time. Nothing here is clever enough to wear out: each line
 * is something a colleague might actually say on walking in.
 */

const LINES = {
  morning: [
    'Good morning, {name}.',
    'Morning, {name}. Where shall we start?',
    'Early start, {name}?',
    'Coffee first, {name}, then the hard problem.',
  ],
  afternoon: [
    'Good afternoon, {name}.',
    'Afternoon, {name}. What are we building?',
    'Back at it, {name}?',
    'What is on your mind, {name}?',
  ],
  evening: [
    'Good evening, {name}.',
    'Evening, {name}. One more thing before you stop?',
    'Still at it, {name}?',
    'Winding down, {name}, or just getting going?',
  ],
  night: [
    'Burning the midnight oil, {name}?',
    'Late one, {name}. What do you need?',
    'The quiet hours, {name}. Good time to think.',
  ],
} as const

export type Register = keyof typeof LINES

export function registerFor(date: Date): Register {
  const h = date.getHours()
  if (h < 5) return 'night'
  if (h < 12) return 'morning'
  if (h < 18) return 'afternoon'
  if (h < 23) return 'evening'
  return 'night'
}

/** "Scotty (dev)" -> "Scotty"; an email or empty name -> null, so the line
 *  is written without a name rather than with a handle in it. */
export function firstName(name: string | null | undefined): string | null {
  if (!name) return null
  const first = name.trim().split(/\s+/)[0]?.replace(/[^\p{L}\p{M}'’-]/gu, '')
  if (!first || first.includes('@')) return null
  return first
}

export function greeting(
  name: string | null | undefined,
  date = new Date(),
  pick: number = Math.random(),
): string {
  const lines = LINES[registerFor(date)]
  const line = lines[Math.min(lines.length - 1, Math.floor(pick * lines.length))]
  const first = firstName(name)
  if (first) return line.replace('{name}', first)
  // No name: drop the ", {name}" clause, keeping the sentence whole.
  return line.replace(/,?\s*\{name\}/, '').replace(/^(\w)/, (c) => c.toUpperCase())
}
