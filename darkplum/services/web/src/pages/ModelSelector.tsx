import { Fragment, useMemo } from 'react'
import type { Model, ModelRouteSummary } from '../types'
import { formatModelName } from '../lib/formatModelName'
import { backendClassChatLabel, backendClassOf } from '../lib/modelRouting'
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

/** Where a routed alias runs. The entry's own class is the default route's
 *  target; when the API tells us the routes differ, say that instead of
 *  promising a place this message may not go. The route list that follows
 *  already says the choice is per message, so this does not repeat it. */
function routedClassLabel(m: Model): string {
  const routes = m.routes ?? []
  const declared = routes
    .map((r) => r.target_backend_class)
    .filter((c): c is NonNullable<typeof c> => Boolean(c))
  const mixed =
    declared.length > 0 && declared.some((c) => c !== backendClassOf(m))
  if (mixed) return 'Where it runs varies by route'
  return backendClassChatLabel(backendClassOf(m))
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
  const groups = useMemo(() => {
    const isAlias = (m: Model) => Boolean(m.alias_of) || m.alias_kind === 'routed'
    const aliases = models.filter(isAlias)
    const direct = models.filter((m) => !isAlias(m))
    return {
      aliases,
      selfHosted: direct.filter((m) => backendClassOf(m) === 'self_hosted'),
      cloud: direct.filter((m) => backendClassOf(m) === 'cloud_managed'),
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
  const isCloud = selected ? backendClassOf(selected) === 'cloud_managed' : false
  const selectedRoutes = selected?.alias_kind === 'routed' ? selected.routes ?? [] : null
  const classLabel = !selected
    ? ''
    : selectedRoutes
      ? routedClassLabel(selected)
      : backendClassChatLabel(backendClassOf(selected))

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
          <optgroup label="Self-hosted">
            {groups.selfHosted.map((m) => (
              <option key={m.id} value={m.id}>{optionLabel(m)}</option>
            ))}
          </optgroup>
        )}
        {groups.cloud.length > 0 && (
          <optgroup label="Cloud-managed">
            {groups.cloud.map((m) => (
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
        className={styles.status}
        role={selected ? undefined : 'status'}
        title={classLabel ? statusTitle(classLabel, selectedRoutes ?? []) : undefined}
      >
        <span
          className={`${styles.dot} ${
            !selected ? styles.dotAmber : isCloud ? styles.dotCloud : styles.dotGreen
          }`}
          aria-hidden="true"
        />
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
