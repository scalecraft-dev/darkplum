import { useMemo, useState } from 'react'
import type { AdminModel, ModelAlias } from '../types'
import { aliasTargetIds } from '../lib/policy'
import {
  backendClassLabel,
  backendClassTooltip,
  backendClassVariant,
  displayClassOf,
  isEgress,
  isReachable,
} from '../lib/modelRouting'
import { formatModelName } from '../lib/formatModelName'
import { useToast } from '../components/Toast'
import { AWS_OPT_IN_LABEL, needsAwsOptIn } from '../lib/dataRetention'
import { Badge, ClassGlyph, OptInGlyph } from '../components/ui'
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

  /** How many ticked models call a vendor API. Counted from the catalogue, so
   *  an id left in the list after its model disappeared is not counted as
   *  anything — it is not reachable either. */
  const tickedEgress = useMemo(
    () => enabled.filter((m) => value.includes(m.model_id) && isEgress(displayClassOf(m))).length,
    [enabled, value],
  )

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
            const cls = displayClassOf(m)
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
                    {/* An allowlist is written model by model, so the class
                        rides on every row: ticking a vendor model is a
                        decision about egress and must never look like ticking
                        a local one. */}
                    <Badge
                      variant={backendClassVariant(cls)}
                      bordered
                      dashed={cls === 'unknown'}
                      title={backendClassTooltip(cls)}
                    >
                      <ClassGlyph cls={cls} />
                      {backendClassLabel(cls)}
                    </Badge>
                    {needsAwsOptIn(m) && (
                      <Badge variant="warning" bordered>
                        <OptInGlyph />
                        {AWS_OPT_IN_LABEL}
                      </Badge>
                    )}
                    {!isReachable(m) && <Badge variant="warning" bordered>Not reachable</Badge>}
                  </span>
                </label>
              </li>
            )
          })
        )}
      </ul>

      {/* The tally, and the one thing about it worth calling out: an allowlist
          is read as a safety measure, so a list that includes models outside
          the account should say so where it is being written, not later. */}
      <p className={styles.count} role="status" aria-live="polite">
        {value.length} of {enabled.length} enabled models ticked
        {tickedEgress > 0 && (
          <span className={styles.countEgress}>
            {' · '}
            {tickedEgress === 1 ? '1 sends prompts' : `${tickedEgress} send prompts`} outside your
            cloud account
          </span>
        )}
      </p>
    </div>
  )
}
