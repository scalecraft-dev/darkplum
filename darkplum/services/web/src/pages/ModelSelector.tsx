import type { Model } from '../types'
import { formatModelName } from '../lib/formatModelName'
import { Skeleton } from '../components/ui/Skeleton'
import styles from './ModelSelector.module.css'

interface Props {
  models: Model[]
  isLoading: boolean
  error: string | null
  value: string
  onChange: (modelId: string) => void
}

export function ModelSelector({ models, isLoading, error, value, onChange }: Props) {
  if (isLoading) {
    return (
      <div className={styles.wrapper}>
        <Skeleton width="140px" height="30px" />
      </div>
    )
  }

  const hasModels = models.length > 0
  const hasError = error !== null && !hasModels

  let dotClass = styles.dotGreen
  let statusText = 'Running in your infrastructure'

  if (hasError) {
    dotClass = styles.dotRed
    statusText = 'Model unavailable — check gateway status'
  } else if (!hasModels) {
    dotClass = styles.dotAmber
    statusText = 'Model starting up'
  }

  return (
    <div className={styles.wrapper}>
      <select
        className={styles.select}
        aria-label="Select model"
        value={hasModels ? value : ''}
        onChange={(e) => onChange(e.target.value)}
        disabled={!hasModels}
      >
        {hasModels ? (
          models.map((m) => (
            <option key={m.id} value={m.id}>
              {formatModelName(m.id)}
            </option>
          ))
        ) : (
          <option value="">{hasError ? 'Unavailable' : 'Model loading...'}</option>
        )}
      </select>
      <span className={styles.status}>
        <span className={`${styles.dot} ${dotClass}`} role="status" aria-label={statusText} />
        <span className={styles.statusLabel}>{statusText}</span>
      </span>
    </div>
  )
}
