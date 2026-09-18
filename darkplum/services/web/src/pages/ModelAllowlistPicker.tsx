import { useMemo, useState } from 'react'
import type { AdminModel, ModelAlias } from '../types'
import { aliasTargetIds } from '../lib/policy'
import { backendClassLabel, backendClassOf, isReachable } from '../lib/modelRouting'
import { formatModelName } from '../lib/formatModelName'
import { useToast } from '../components/Toast'
import { Badge } from '../components/ui'
import styles from './ModelAllowlistPicker.module.css'

interface Props {
  /** Every discovered model. Only the enabled ones can be allowed. */
  models: AdminModel[]
  /** Aliases, offered as a shortcut for filling the list. */
  aliases: ModelAlias[]
  /** The model ids currently ticked. */
  value: string[]
  onChange: (next: string[]) => void
  /** Names the thing being edited, for the search field's label. */
  label: string
}

/**
 * The one control for writing a model allowlist — used by the organisation's
 * policy card and by the per-principal drawer, so the two never drift apart.
 *
 * An allowlist stores concrete model ids and never an alias. The alias buttons
 * expand to ids at the moment they are clicked, so editing an alias afterwards
 * cannot silently widen a policy that was written against it.
 */
export function ModelAllowlistPicker({ models, aliases, value, onChange, label }: Props) {
  const { toast } = useToast()
  const [search, setSearch] = useState('')

  const enabled = useMemo(
    () => models.filter((m) => m.enabled).sort((a, b) => a.model_id.localeCompare(b.model_id)),
    [models],
  )

  const aliasHelpers = useMemo(
    () =>
      aliases
        .map((a) => ({ alias: a.alias, targets: aliasTargetIds(a) }))
        .filter((a) => a.targets.length > 0),
    [aliases],
  )

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    if (!q) return enabled
    return enabled.filter(
      (m) =>
        m.model_id.toLowerCase().includes(q) ||
        (m.display_name?.toLowerCase().includes(q) ?? false) ||
        formatModelName(m.model_id).toLowerCase().includes(q),
    )
  }, [enabled, search])

  const toggle = (modelId: string) => {
    const next = value.includes(modelId)
      ? value.filter((id) => id !== modelId)
      : [...value, modelId]
    onChange(next.sort())
  }

  const addAliasTargets = (alias: string, targets: string[]) => {
    const added = targets.filter((t) => !value.includes(t))
    onChange([...new Set([...value, ...targets])].sort())
    toast(
      added.length === 0
        ? `Every target of ${alias} was already allowed`
        : `Added ${added.length} ${added.length === 1 ? 'model' : 'models'} from ${alias}`,
      'success',
    )
  }

  return (
    <div className={styles.picker}>
      <input
        type="search"
        className={styles.search}
        value={search}
        onChange={(e) => setSearch(e.target.value)}
        placeholder="Search models"
        aria-label={`Search models to allow for ${label}`}
      />

      {aliasHelpers.length > 0 && (
        <div className={styles.aliasRow}>
          <span className={styles.aliasRowLabel}>Add every target of</span>
          {aliasHelpers.map((a) => (
            <button
              key={a.alias}
              type="button"
              className={styles.aliasBtn}
              onClick={() => addAliasTargets(a.alias, a.targets)}
              title={a.targets.join('\n')}
            >
              <code>{a.alias}</code>
              <span className={styles.aliasCount}>{a.targets.length}</span>
            </button>
          ))}
        </div>
      )}

      <ul className={styles.modelList} role="list">
        {filtered.length === 0 ? (
          <li className={styles.modelEmpty}>
            {enabled.length === 0
              ? 'No models are enabled, so there is nothing to allow yet.'
              : `No enabled model matches “${search}”.`}
          </li>
        ) : (
          filtered.map((m) => {
            const cls = backendClassOf(m)
            return (
              <li key={m.id} className={styles.modelItem}>
                <label className={styles.modelLabel}>
                  <input
                    type="checkbox"
                    className={styles.checkbox}
                    checked={value.includes(m.model_id)}
                    onChange={() => toggle(m.model_id)}
                  />
                  <span className={styles.modelText}>
                    <span className={styles.modelName}>
                      {m.display_name ?? formatModelName(m.model_id)}
                    </span>
                    <code className={styles.modelId}>{m.model_id}</code>
                  </span>
                  <span className={styles.modelFlags}>
                    <Badge variant={cls === 'cloud_managed' ? 'info' : 'neutral'} bordered>
                      {backendClassLabel(cls)}
                    </Badge>
                    {!isReachable(m) && <Badge variant="warning" bordered>Not reachable</Badge>}
                  </span>
                </label>
              </li>
            )
          })
        )}
      </ul>

      <p className={styles.count} role="status" aria-live="polite">
        {value.length} of {enabled.length} enabled models ticked
      </p>
    </div>
  )
}
