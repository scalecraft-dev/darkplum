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

/** Label for one entry. Aliases lead with the alias and name their target. */
function optionLabel(m: Model): string {
  if (m.alias_of) return `${m.id} — ${formatModelName(m.alias_of)}`
  return formatModelName(m.id)
}

export function ModelSelector({ models, isLoading, error, value, onChange }: Props) {
  const groups = useMemo(() => {
    const aliases = models.filter((m) => m.alias_of)
    const direct = models.filter((m) => !m.alias_of)
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
          {selected ? backendClassChatLabel(backendClassOf(selected)) : 'No model selected'}
        </span>
      </span>

      {selected?.alias_of && (
        <span className={styles.aliasTarget}>
          Shortcut for <code className={styles.aliasTargetId}>{selected.alias_of}</code>
        </span>
      )}
    </div>
  )
}
