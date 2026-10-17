import { Fragment, useMemo } from 'react'
import type { Model, ModelRouteSummary } from '../types'
import { formatModelName } from '../lib/formatModelName'
import {
  backendClassChatLabel,
  displayClassOf,
  isEgress,
  vendorLabel,
} from '../lib/modelRouting'
import { EgressGlyph } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import styles from './ModelSelector.module.css'

interface Props {
  models: Model[]
  isLoading: boolean
  error: string | null
  value: string
  onChange: (modelId: string) => void
}

/** Label for one entry. Aliases lead with the alias; a static one names its
 *  target, a routed one says how many models its routes can reach. Naming a
 *  single model there would claim a decision the routes have not made yet. */
function optionLabel(m: Model): string {
  if (m.alias_kind === 'routed') {
    const count = m.routes?.length ?? 0
    return count > 0
      ? `${m.id} — routes to one of ${count} ${count === 1 ? 'model' : 'models'}`
      : `${m.id} — routed`
  }
  if (m.alias_of) return `${m.id} — ${formatModelName(m.alias_of)}`
  return formatModelName(m.id)
}

/**
 * Where a routed alias runs.
 *
 * A routed alias answers on one of the routes an administrator wrote, and
 * which one is decided per message. The entry's own class describes the
 * default route's target, so it is only the whole answer when every route
 * agrees.
 *
 * Precedence when they do not: if ANY route can leave the account, that is
 * what this says, regardless of where the default route happens to point. A
 * person deciding whether to paste something in needs the worst case, not the
 * likeliest one — "runs in your infrastructure" on an alias that might go to
 * Anthropic is the one sentence this product must never print.
 */
function routedClassLabel(m: Model): { text: string; egress: boolean } {
  const routes = m.routes ?? []
  // Every route counts, and a route that names no class is 'unknown' — the
  // entry's own class is only the default route's, so it must never stand in
  // for routes the API did describe.
  const declared = routes.map((r) => displayClassOf({ backend_class: r.backend_class }))
  const own = displayClassOf(m)
  const classes = declared.length > 0 ? declared : [own]

  if (classes.some(isEgress)) {
    // The vendor cannot be named: routes carry a class, not a provider, and
    // naming the default route's vendor would be wrong for the others.
    return {
      text:
        classes.every(isEgress) && classes.length === declared.length && declared.length > 0
          ? 'Sent outside your cloud account'
          : 'Some routes leave your cloud account',
      egress: true,
    }
  }
  if (classes.some((c) => c !== classes[0])) {
    return { text: 'Where it runs varies by route', egress: false }
  }
  return { text: backendClassChatLabel(classes[0]), egress: false }
}

/** Where one concrete model runs, in the words its own backend earns. The
 *  vendor is named from `owned_by`, which on an external model is the vendor. */
function directClassLabel(m: Model): { text: string; egress: boolean } {
  const cls = displayClassOf(m)
  return { text: backendClassChatLabel(cls, m.owned_by), egress: isEgress(cls) }
}

/** The full sentence for the status line's title: what the header truncates,
 *  plus the route descriptions there is never room for. Said once — the visible
 *  line is the same text, shortened, so nothing is announced twice. */
function statusTitle(classLabel: string, routes: ModelRouteSummary[]): string {
  if (routes.length === 0) return classLabel
  const names = routes.map((r) => r.route_name).join(', ')
  const described = routes
    .filter((r) => r.description)
    .map((r) => `${r.route_name}: ${r.description}`)
  const head = `${classLabel} · routes: ${names}`
  return described.length > 0 ? `${head}\n${described.join('\n')}` : head
}

export function ModelSelector({ models, isLoading, error, value, onChange }: Props) {
  /**
   * Four groups, in the order a person cares about them: the names their
   * administrator invented, then the three places a model can run, closest to
   * the organisation first. A vendor's models sit in a group named after the
   * vendor, because "OpenAI" is a more useful heading in a menu than "External
   * API" — the class vocabulary is for the administrator writing the policy,
   * not for the person picking something to type at.
   */
  const groups = useMemo(() => {
    const isAlias = (m: Model) => Boolean(m.alias_of) || m.alias_kind === 'routed'
    const aliases = models.filter(isAlias)
    const direct = models.filter((m) => !isAlias(m))
    const external = direct.filter((m) => displayClassOf(m) === 'external_api')
    const vendors = [...new Set(external.map((m) => m.owned_by))].sort()
    return {
      aliases,
      selfHosted: direct.filter((m) => displayClassOf(m) === 'self_hosted'),
      cloud: direct.filter((m) => displayClassOf(m) === 'cloud_managed'),
      /** One optgroup per vendor, so the heading is the vendor's name. */
      externalByVendor: vendors.map((v) => ({
        vendor: v,
        models: external.filter((m) => m.owned_by === v),
      })),
      /** A model whose class nobody has reported. It is on the list because
       *  the policy let it through, and it is grouped apart because the one
       *  honest thing to say about it is that we do not know. */
      unknown: direct.filter((m) => displayClassOf(m) === 'unknown'),
    }
  }, [models])

  if (isLoading && models.length === 0) {
    return (
      <div className={styles.wrapper}>
        <Skeleton width="180px" height="30px" />
      </div>
    )
  }

  // No picker without models: an empty <select> is a dead control. The body of
  // the page carries the explanation and the next step.
  if (models.length === 0) {
    const unreachable = error !== null
    return (
      <div className={styles.wrapper}>
        <span className={styles.status} role="status">
          <span
            className={`${styles.dot} ${unreachable ? styles.dotRed : styles.dotAmber}`}
            aria-hidden="true"
          />
          <span className={styles.statusLabel}>
            {unreachable ? 'Model gateway unreachable' : 'No models available'}
          </span>
        </span>
      </div>
    )
  }

  const selected = models.find((m) => m.id === value) ?? null
  const selectedRoutes = selected?.alias_kind === 'routed' ? selected.routes ?? [] : null
  const where = !selected
    ? null
    : selectedRoutes
      ? routedClassLabel(selected)
      : directClassLabel(selected)
  const classLabel = where?.text ?? ''
  const selectedClass = selected ? displayClassOf(selected) : null

  /** Four states for the dot, and the label always says the same thing the dot
   *  does — the dot is a shortcut for people who already know it, never the
   *  only carrier of the state. */
  const dotClass = !selected
    ? styles.dotAmber
    : where?.egress
      ? styles.dotEgress
      : selectedClass === 'unknown'
        ? styles.dotUnknown
        : selectedClass === 'cloud_managed'
          ? styles.dotCloud
          : styles.dotGreen

  return (
    <div className={styles.wrapper}>
      <select
        className={styles.select}
        aria-label="Model"
        value={selected ? value : ''}
        onChange={(e) => onChange(e.target.value)}
      >
        {!selected && <option value="">Choose a model</option>}
        {groups.aliases.length > 0 && (
          <optgroup label="Shortcuts">
            {groups.aliases.map((m) => (
              <option key={m.id} value={m.id}>{optionLabel(m)}</option>
            ))}
          </optgroup>
        )}
        {groups.selfHosted.length > 0 && (
          <optgroup label="Your infrastructure">
            {groups.selfHosted.map((m) => (
              <option key={m.id} value={m.id}>{optionLabel(m)}</option>
            ))}
          </optgroup>
        )}
        {groups.cloud.length > 0 && (
          <optgroup label="Your cloud account">
            {groups.cloud.map((m) => (
              <option key={m.id} value={m.id}>{optionLabel(m)}</option>
            ))}
          </optgroup>
        )}
        {/* An <optgroup> label is the only place a warning can live inside a
            native select, so it carries the whole fact rather than the
            vendor's name alone. */}
        {groups.externalByVendor.map(({ vendor, models: vendorModels }) => (
          <optgroup key={vendor} label={`${vendorLabel(vendor)} — leaves your cloud account`}>
            {vendorModels.map((m) => (
              <option key={m.id} value={m.id}>{optionLabel(m)}</option>
            ))}
          </optgroup>
        ))}
        {groups.unknown.length > 0 && (
          <optgroup label="Where these run is not known yet">
            {groups.unknown.map((m) => (
              <option key={m.id} value={m.id}>{optionLabel(m)}</option>
            ))}
          </optgroup>
        )}
      </select>

      {/* With nothing selected there is no backend to describe, and naming one
          anyway would claim a routing decision the person has not made yet.
          A routed alias answers on one of the routes its administrator wrote;
          which one is decided per message and named under the reply, so this
          line lists the routes rather than promising one of them. The list is
          the first thing to give way when the header is short of room. */}
      <span
        className={`${styles.status} ${where?.egress ? styles.statusEgress : ''}`}
        role={selected ? undefined : 'status'}
        title={classLabel ? statusTitle(classLabel, selectedRoutes ?? []) : undefined}
      >
        <span className={`${styles.dot} ${dotClass}`} aria-hidden="true" />
        {where?.egress && <EgressGlyph className={styles.statusGlyph} />}
        <span className={styles.statusLabel}>{classLabel || 'No model selected'}</span>
        {selectedRoutes && selectedRoutes.length > 0 && (
          <span className={styles.statusRoutes}>
            {'· routes: '}
            {selectedRoutes.map((r, i) => (
              <Fragment key={r.route_name}>
                {i > 0 && ', '}
                <code className={styles.aliasTargetId}>{r.route_name}</code>
              </Fragment>
            ))}
          </span>
        )}
      </span>

      {!selectedRoutes && selected?.alias_of && (
        <span className={styles.aliasTarget}>
          Shortcut for <code className={styles.aliasTargetId}>{selected.alias_of}</code>
        </span>
      )}
    </div>
  )
}
