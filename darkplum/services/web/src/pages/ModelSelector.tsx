import { useMemo } from 'react'
import type { Model } from '../types'
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
 *  promising a place this message may not go. */
function routedClassLabel(m: Model): string {
  const routes = m.routes ?? []
  const declared = routes
    .map((r) => r.target_backend_class)
    .filter((c): c is NonNullable<typeof c> => Boolean(c))
  const mixed =
    declared.length > 0 && declared.some((c) => c !== backendClassOf(m))
  if (mixed) return 'Where it runs varies by route'
  return `${backendClassChatLabel(backendClassOf(m))} · default route`
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
          anyway would claim a routing decision the person has not made yet. */}
      <span className={styles.status} role={selected ? undefined : 'status'}>
        <span
          className={`${styles.dot} ${
            !selected ? styles.dotAmber : isCloud ? styles.dotCloud : styles.dotGreen
          }`}
          aria-hidden="true"
        />
        <span className={styles.statusLabel}>
          {!selected
            ? 'No model selected'
            : selectedRoutes
              ? routedClassLabel(selected)
              : backendClassChatLabel(backendClassOf(selected))}
        </span>
      </span>

      {/* A routed alias answers on one of the routes its administrator wrote.
          Which one is decided per message and named under the reply, so the
          picker says how many there are rather than promising one. */}
      {selectedRoutes ? (
        selectedRoutes.length > 0 && (
          <span className={styles.aliasTarget}>
            <span aria-hidden="true">
              Routes to one of {selectedRoutes.length}{' '}
              {selectedRoutes.length === 1 ? 'model' : 'models'}:{' '}
              {selectedRoutes.map((r, i) => (
                <span key={r.route_name}>
                  {i > 0 && ', '}
                  <code className={styles.aliasTargetId} title={r.description}>
                    {r.route_name}
                  </code>
                </span>
              ))}
            </span>
            <span className={styles.srOnly}>
              {`Routes to one of ${selectedRoutes.length} models. ` +
                selectedRoutes.map((r) => `${r.route_name}: ${r.description}`).join('. ')}
            </span>
          </span>
        )
      ) : (
        selected?.alias_of && (
          <span className={styles.aliasTarget}>
            Shortcut for <code className={styles.aliasTargetId}>{selected.alias_of}</code>
          </span>
        )
      )}
    </div>
  )
}
