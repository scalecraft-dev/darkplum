import { useEffect, useMemo, useRef, useState } from 'react'
import type { AdminModel, ModelAlias, Residency } from '../types'
import { ApiError } from '../lib/api'
import { ALIAS_HINT, backendClassOf, validateAlias } from '../lib/modelRouting'
import { useToast } from '../components/Toast'
import { Badge, Button, Input, Modal, Table, TableEmpty } from '../components/ui'
import { CopyButton } from '../components/ui/CopyButton'
import { Skeleton } from '../components/ui/Skeleton'
import styles from './AliasesSection.module.css'

interface Props {
  aliases: ModelAlias[]
  models: AdminModel[]
  loading: boolean
  error: string | null
  residency: Residency
  onRetry: () => void
  onSave: (alias: string, targetModelId: string) => Promise<void>
  onDelete: (alias: string) => Promise<void>
}

type AliasHealth =
  | { kind: 'ok' }
  | { kind: 'missing' }
  | { kind: 'disabled' }
  | { kind: 'blocked' }

function healthOf(a: ModelAlias, residency: Residency): AliasHealth {
  if (!a.target_exists) return { kind: 'missing' }
  if (!a.target_enabled) return { kind: 'disabled' }
  if (residency === 'self_hosted_only' && a.target_backend_class === 'cloud_managed') {
    return { kind: 'blocked' }
  }
  return { kind: 'ok' }
}

const HEALTH_COPY: Record<AliasHealth['kind'], { label: string; variant: 'success' | 'warning' | 'danger'; hint: string }> = {
  ok: {
    label: 'Active',
    variant: 'success',
    hint: '',
  },
  missing: {
    label: 'Target missing',
    variant: 'danger',
    hint: 'No model has this ID. Requests using this alias are refused.',
  },
  disabled: {
    label: 'Target disabled',
    variant: 'warning',
    hint: 'Enable the target model, or point this alias at one that is on.',
  },
  blocked: {
    label: 'Blocked by policy',
    variant: 'warning',
    hint: 'The routing policy allows self-hosted models only, and this target is cloud-managed.',
  },
}

// ---------------------------------------------------------------------------
// Delete — two-step confirm, matching the pattern used for users.
// ---------------------------------------------------------------------------

function DeleteAliasButton({ alias, onDelete }: { alias: string; onDelete: (alias: string) => Promise<void> }) {
  const [confirming, setConfirming] = useState(false)
  const [loading, setLoading] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => () => { if (timer.current) clearTimeout(timer.current) }, [])

  const handleClick = async () => {
    if (!confirming) {
      setConfirming(true)
      timer.current = setTimeout(() => setConfirming(false), 3000)
      return
    }
    if (timer.current) clearTimeout(timer.current)
    setLoading(true)
    try {
      await onDelete(alias)
    } finally {
      setLoading(false)
      setConfirming(false)
    }
  }

  return (
    <button
      type="button"
      className={`${styles.rowBtn} ${confirming ? styles.rowBtnConfirm : ''}`}
      onClick={() => void handleClick()}
      disabled={loading}
      aria-label={confirming ? `Confirm deleting alias ${alias}` : `Delete alias ${alias}`}
    >
      {loading ? 'Deleting...' : confirming ? 'Confirm?' : 'Delete'}
    </button>
  )
}

// ---------------------------------------------------------------------------
// Create / retarget dialog
// ---------------------------------------------------------------------------

interface DialogProps {
  open: boolean
  editing: ModelAlias | null
  models: AdminModel[]
  residency: Residency
  existing: ModelAlias[]
  onClose: () => void
  onSave: (alias: string, targetModelId: string) => Promise<void>
}

function AliasDialog({ open, editing, models, residency, existing, onClose, onSave }: DialogProps) {
  const [alias, setAlias] = useState('')
  const [target, setTarget] = useState('')
  const [aliasError, setAliasError] = useState('')
  const [targetError, setTargetError] = useState('')
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    if (!open) return
    setAlias(editing?.alias ?? '')
    setTarget(editing?.target_model_id ?? '')
    setAliasError('')
    setTargetError('')
  }, [open, editing])

  const grouped = useMemo(() => {
    const sorted = [...models].sort((a, b) => a.model_id.localeCompare(b.model_id))
    return {
      selfHosted: sorted.filter((m) => backendClassOf(m) === 'self_hosted'),
      cloud: sorted.filter((m) => backendClassOf(m) === 'cloud_managed'),
    }
  }, [models])

  const selected = models.find((m) => m.model_id === target) ?? null
  const selectedClass = selected ? backendClassOf(selected) : null

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const trimmed = alias.trim()
    const formatError = validateAlias(trimmed)
    if (formatError) {
      setAliasError(formatError)
      return
    }
    if (!editing && existing.some((a) => a.alias === trimmed)) {
      setAliasError('This alias already exists. Retarget it instead.')
      return
    }
    if (!target) {
      setTargetError('Choose the model this alias points at.')
      return
    }
    setAliasError('')
    setTargetError('')
    setSaving(true)
    try {
      await onSave(trimmed, target)
      onClose()
    } catch (err) {
      // Show the server's own message, against the field it belongs to.
      const status = err instanceof ApiError ? err.status : 0
      const message = (err as Error).message
      if (status === 409) setAliasError(message)
      else if (status === 404) setTargetError(message)
      else if (status === 400 && /alias/i.test(message)) setAliasError(message)
      else setTargetError(message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={editing ? `Point ${editing.alias} at another model` : 'Create alias'}
      description={
        editing
          ? 'Anyone already using this alias moves to the new target on their next message.'
          : 'An alias is a stable name people can pick in chat. You can repoint it later without anyone changing their settings.'
      }
    >
      <form className={styles.form} onSubmit={(e) => void handleSubmit(e)}>
        <div className={styles.field}>
          <Input
            label="Alias"
            type="text"
            value={alias}
            onChange={(e) => { setAlias(e.target.value); setAliasError('') }}
            placeholder="fast"
            error={aliasError}
            disabled={Boolean(editing)}
            autoFocus={!editing}
            spellCheck={false}
            autoComplete="off"
            className={styles.monoInput}
          />
          {!editing && <p className={styles.fieldHint}>{ALIAS_HINT}</p>}
        </div>

        <div className={styles.field}>
          <label className={styles.fieldLabel} htmlFor="alias-target">Points at</label>
          <select
            id="alias-target"
            className={styles.select}
            value={target}
            onChange={(e) => { setTarget(e.target.value); setTargetError('') }}
            autoFocus={Boolean(editing)}
          >
            <option value="">Choose a model</option>
            {grouped.selfHosted.length > 0 && (
              <optgroup label="Self-hosted">
                {grouped.selfHosted.map((m) => (
                  <option key={m.id} value={m.model_id}>
                    {m.model_id}{m.enabled ? '' : ' (disabled)'}
                  </option>
                ))}
              </optgroup>
            )}
            {grouped.cloud.length > 0 && (
              <optgroup label="Cloud-managed">
                {grouped.cloud.map((m) => (
                  <option key={m.id} value={m.model_id}>
                    {m.model_id}{m.enabled ? '' : ' (disabled)'}
                  </option>
                ))}
              </optgroup>
            )}
          </select>
          {targetError && <span className={styles.fieldError}>{targetError}</span>}
        </div>

        {selected && !selected.enabled && (
          <p className={styles.formWarning}>
            This model is turned off, so the alias is refused until you enable it.
          </p>
        )}
        {selectedClass === 'cloud_managed' && residency === 'self_hosted_only' && (
          <p className={styles.formWarning}>
            The routing policy allows self-hosted models only, so this alias is refused until the
            policy changes.
          </p>
        )}

        <div className={styles.formFooter}>
          <Button type="button" variant="ghost" onClick={onClose}>Cancel</Button>
          <Button type="submit" disabled={saving}>
            {saving ? 'Saving...' : editing ? 'Save target' : 'Create alias'}
          </Button>
        </div>
      </form>
    </Modal>
  )
}

// ---------------------------------------------------------------------------
// Section
// ---------------------------------------------------------------------------

export function AliasesSection({
  aliases,
  models,
  loading,
  error,
  residency,
  onRetry,
  onSave,
  onDelete,
}: Props) {
  const { toast } = useToast()
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<ModelAlias | null>(null)

  const openCreate = () => { setEditing(null); setDialogOpen(true) }
  const openEdit = (a: ModelAlias) => { setEditing(a); setDialogOpen(true) }

  const handleDelete = async (alias: string) => {
    try {
      await onDelete(alias)
      toast(`Alias ${alias} deleted`, 'success')
    } catch (err) {
      toast((err as Error).message, 'error')
    }
  }

  return (
    <section className={styles.card} aria-labelledby="aliases-heading">
      <div className={styles.head}>
        <div>
          <h2 id="aliases-heading" className={styles.title}>Aliases</h2>
          <p className={styles.subtitle}>
            Stable names that point at a model. People pick the alias in chat; you decide where it
            goes.
          </p>
        </div>
        <Button
          size="sm"
          onClick={openCreate}
          disabled={loading || models.length === 0}
          title={models.length === 0 ? 'No models have been discovered yet' : undefined}
        >
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <line x1="12" y1="5" x2="12" y2="19" />
            <line x1="5" y1="12" x2="19" y2="12" />
          </svg>
          Add alias
        </Button>
      </div>

      {error ? (
        <div className={styles.error} role="alert">
          <p className={styles.errorTitle}>Aliases could not be loaded</p>
          <p className={styles.errorDetail}>{error}</p>
          <Button variant="secondary" size="sm" onClick={onRetry}>Try again</Button>
        </div>
      ) : (
        <div className={styles.tableWrap}>
          <Table>
            <thead>
              <tr>
                <th>Alias</th>
                <th>Points at</th>
                <th>Status</th>
                <th className={styles.actionsCol}></th>
              </tr>
            </thead>
            <tbody>
              {loading ? (
                Array.from({ length: 2 }).map((_, i) => (
                  <tr key={i}>
                    <td><Skeleton width="90px" height="14px" /></td>
                    <td><Skeleton width="200px" height="14px" /></td>
                    <td><Skeleton width="60px" height="18px" borderRadius="var(--radius-full)" /></td>
                    <td className={styles.actionsCol}><Skeleton width="60px" height="20px" /></td>
                  </tr>
                ))
              ) : aliases.length === 0 ? (
                <TableEmpty colSpan={4}>
                  <div className={styles.empty}>
                    <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                      <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71" />
                      <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71" />
                    </svg>
                    <span className={styles.emptyTitle}>No aliases yet</span>
                    <span className={styles.emptyHint}>
                      Give a model a short name such as <code className={styles.emptyCode}>fast</code>{' '}
                      and swap the model behind it whenever you like.
                    </span>
                  </div>
                </TableEmpty>
              ) : (
                aliases.map((a) => {
                  const health = healthOf(a, residency)
                  const copy = HEALTH_COPY[health.kind]
                  return (
                    <tr key={a.alias} className={health.kind === 'ok' ? '' : styles.rowFlagged}>
                      <td>
                        <div className={styles.idCell}>
                          <code className={styles.alias}>{a.alias}</code>
                          <CopyButton value={a.alias} label="Copy" />
                        </div>
                      </td>
                      <td>
                        <div className={styles.idCell}>
                          <code className={styles.targetId}>{a.target_model_id}</code>
                          <CopyButton value={a.target_model_id} label="Copy" />
                        </div>
                      </td>
                      <td>
                        <Badge variant={copy.variant} bordered={health.kind !== 'ok'}>
                          {copy.label}
                        </Badge>
                        {copy.hint && <p className={styles.statusHint}>{copy.hint}</p>}
                      </td>
                      <td className={styles.actionsCol}>
                        <div className={styles.rowActions}>
                          <button
                            type="button"
                            className={styles.rowBtn}
                            onClick={() => openEdit(a)}
                            aria-label={`Change the target of alias ${a.alias}`}
                          >
                            Retarget
                          </button>
                          <DeleteAliasButton alias={a.alias} onDelete={handleDelete} />
                        </div>
                      </td>
                    </tr>
                  )
                })
              )}
            </tbody>
          </Table>
        </div>
      )}

      <AliasDialog
        open={dialogOpen}
        editing={editing}
        models={models}
        residency={residency}
        existing={aliases}
        onClose={() => setDialogOpen(false)}
        onSave={onSave}
      />
    </section>
  )
}
