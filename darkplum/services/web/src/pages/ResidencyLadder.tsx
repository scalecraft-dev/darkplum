import { useId } from 'react'
import type { Residency } from '../types'
import { DEFAULT_RESIDENCY, RESIDENCY_LADDER, residencyRank } from '../lib/policy'
import { EgressGlyph, LockGlyph } from '../components/ui'
import styles from './ResidencyLadder.module.css'

/**
 * The one control for choosing where requests may run.
 *
 * Three rungs, loosest first, in the order the ladder actually is — a range
 * reads outward-in, and reversing it on one screen and not the other is how two
 * administrators end up describing the same policy differently. The
 * organisation's card and the per-principal drawer both render this, so the
 * words and the order cannot drift apart.
 *
 * A rung a principal may not reach is disabled rather than hidden. Hiding it
 * answers "why can't I choose that?" by pretending the question was never
 * asked; disabling it with the reason attached answers it. The reason is tied
 * to the input with aria-describedby, so it is read out with the option rather
 * than sitting nearby as unrelated text.
 */

export interface LadderRung {
  value: Residency
  /** Why this rung cannot be chosen here, or null when it can. A disabled rung
   *  always has one: a control that refuses without saying why is a dead end. */
  deadReason: string | null
}

interface Props {
  /** The rung currently chosen. null is the inherit option, and is only ever
   *  reachable when `inherit` below is given. */
  value: Residency | null
  onChange: (value: Residency | null) => void
  /** Radio group name — unique per form on the page. */
  name: string
  /**
   * Offers "inherit" as the first choice, above the rungs. Present in the
   * per-principal drawer, where not deciding is itself a decision worth being
   * able to make and to return to; absent on the organisation's card, which
   * has nothing above it to inherit from.
   */
  inherit?: { label: string; hint?: string }
  /** Which rungs this surface offers, and which of them are dead. Given in
   *  ladder order; anything not listed is not rendered. */
  rungs: LadderRung[]
  /** The whole group is read-only while a save is in flight. */
  disabled?: boolean
  /** The question the rungs answer. Rendered as the group's legend. */
  legend: string
  /** Marks which rung is what Darkplum ships with, so an administrator can
   *  tell a decision from a default. Off in the per-principal drawer, where
   *  every rung is a decision someone made. */
  showDefault?: boolean
}

export function ResidencyLadder({
  value,
  onChange,
  name,
  inherit,
  rungs,
  disabled = false,
  legend,
  showDefault = false,
}: Props) {
  const scope = useId()
  const offered = RESIDENCY_LADDER.filter((r) => rungs.some((x) => x.value === r.value))

  return (
    <fieldset className={styles.field}>
      <legend className={styles.legend}>{legend}</legend>
      <div className={styles.rungs} role="radiogroup" aria-label={legend}>
        {inherit && (
          <label
            className={[styles.rung, value === null ? styles.rungChecked : '']
              .filter(Boolean)
              .join(' ')}
          >
            <span className={styles.rungTop}>
              <input
                className={styles.radio}
                type="radio"
                name={name}
                checked={value === null}
                disabled={disabled}
                aria-label={inherit.label}
                aria-describedby={inherit.hint ? `${scope}-inherit-hint` : undefined}
                onChange={() => onChange(null)}
              />
              <span className={styles.rungLabel} aria-hidden="true">{inherit.label}</span>
            </span>
            {inherit.hint && (
              <span className={styles.rungConsequence} id={`${scope}-inherit-hint`}>
                {inherit.hint}
              </span>
            )}
          </label>
        )}
        {offered.map((rung) => {
          const dead = rungs.find((r) => r.value === rung.value)?.deadReason ?? null
          const checked = value === rung.value
          const consequenceId = `${scope}-${rung.value}-consequence`
          const reasonId = `${scope}-${rung.value}-reason`
          const isDefault = showDefault && rung.value === DEFAULT_RESIDENCY
          // Only the loosest rung sends anything out of the account; only the
          // strictest keeps everything in the cluster. The middle rung is the
          // ordinary case and is marked as nothing.
          const leaving = residencyRank(rung.value) === 0
          const sealed = rung.value === 'self_hosted_only'
          return (
            <label
              key={rung.value}
              className={[
                styles.rung,
                checked ? styles.rungChecked : '',
                dead ? styles.rungDead : '',
              ]
                .filter(Boolean)
                .join(' ')}
            >
              <span className={styles.rungTop}>
                <input
                  className={styles.radio}
                  type="radio"
                  name={name}
                  value={rung.value}
                  checked={checked}
                  disabled={disabled || dead !== null}
                  /* The whole card is the label, so without this the
                     accessible name would be the rung plus both paragraphs
                     under it — and the dead reason would then be announced
                     twice, once as name and once as description. The name is
                     the rung; everything explaining it is a description. */
                  aria-label={rung.label}
                  aria-describedby={[consequenceId, dead ? reasonId : '']
                    .filter(Boolean)
                    .join(' ')}
                  onChange={() => onChange(rung.value)}
                />
                <span className={styles.rungLabel} aria-hidden="true">
                  {leaving && <EgressGlyph className={styles.rungGlyph} />}
                  {sealed && <LockGlyph className={styles.rungGlyph} />}
                  {rung.label}
                </span>
                {isDefault && (
                  <span className={styles.defaultChip}>
                    Default
                    <span className={styles.srOnly}> — this is what Darkplum ships with</span>
                  </span>
                )}
              </span>
              <span className={styles.rungConsequence} id={consequenceId}>
                {rung.consequence}
              </span>
              {dead && (
                <span className={styles.rungDeadReason} id={reasonId}>
                  {dead}
                </span>
              )}
            </label>
          )
        })}
      </div>
    </fieldset>
  )
}
