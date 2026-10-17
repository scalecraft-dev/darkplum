import { useCallback, useEffect, useMemo, useState } from 'react'
import { deleteModelPrice, listModelPrices, putModelPrice } from '../lib/api'
import { compareProviders, providerLabel } from '../lib/modelRouting'
import { formatPerMtok, parsePriceInput, seedNote } from '../lib/spend'
import type { ModelPrice, ModelPriceInput, ModelPricesResult } from '../types'
import { useToast } from '../components/Toast'
import { Badge, Button, Input, Modal, Select, Skeleton, Table, TableEmpty } from '../components/ui'
import styles from './PricesTab.module.css'

/** Providers offered in the form even before any row names them. */
const KNOWN_PROVIDERS = ['vllm', 'lmstudio', 'bedrock', 'anthropic', 'openai']

const PRICE_FIELDS = [
  { key: 'input_per_mtok',       label: 'Input',       optional: false },
  { key: 'output_per_mtok',      label: 'Output',      optional: false },
  { key: 'cache_read_per_mtok',  label: 'Cache read',  optional: true },
  { key: 'cache_write_per_mtok', label: 'Cache write', optional: true },
] as const
type PriceKey = (typeof PRICE_FIELDS)[number]['key']

function isInForce(r: ModelPrice, now: number): boolean {
  return !r.effective_to || Date.parse(r.effective_to) > now
}

function shortDate(iso: string | null): string {
  if (!iso) return ''
  const d = new Date(iso)
  return Number.isNaN(d.getTime())
    ? iso
    : d.toLocaleDateString('en-US', { year: 'numeric', month: 'short', day: 'numeric', timeZone: 'UTC' })
}

function effectiveText(r: ModelPrice): string {
  return r.effective_to
    ? `${shortDate(r.effective_from)} – ${shortDate(r.effective_to)}`
    : `Since ${shortDate(r.effective_from)}`
}

// ---------------------------------------------------------------------------
// Form
// ---------------------------------------------------------------------------

type FormState = {
  provider: string
  model_pattern: string
  region: string
  notes: string
} & Record<PriceKey, string>

function formFrom(row: ModelPrice | null): FormState {
  const str = (v: string | number | null | undefined) => (v === null || v === undefined ? '' : String(v))
  return {
    provider: row?.provider ?? 'anthropic',
    model_pattern: row?.model_pattern ?? '',
    region: row?.region ?? '',
    notes: row?.notes ?? '',
    input_per_mtok: str(row?.input_per_mtok),
    output_per_mtok: str(row?.output_per_mtok),
    cache_read_per_mtok: str(row?.cache_read_per_mtok),
    cache_write_per_mtok: str(row?.cache_write_per_mtok),
  }
}

function PriceForm({
  replacing,
  providers,
  saving,
  onCancel,
  onSubmit,
}: {
  replacing: ModelPrice | null
  providers: string[]
  saving: boolean
  onCancel: () => void
  onSubmit: (input: ModelPriceInput) => void
}) {
  const [form, setForm] = useState<FormState>(() => formFrom(replacing))
  const [errors, setErrors] = useState<Partial<Record<keyof FormState, string>>>({})
  const set = (k: keyof FormState) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm((f) => ({ ...f, [k]: e.target.value }))

  function submit(e: React.FormEvent) {
    e.preventDefault()
    const next: Partial<Record<keyof FormState, string>> = {}
    const prices: Partial<Record<PriceKey, string | null>> = {}
    for (const f of PRICE_FIELDS) {
      const r = parsePriceInput(form[f.key], f.optional)
      if (r.ok) prices[f.key] = r.value
      else next[f.key] = r.error
    }
    if (!form.model_pattern.trim()) next.model_pattern = 'Required'
    setErrors(next)
    if (Object.keys(next).length > 0) return
    onSubmit({
      ...(replacing ? { id: replacing.id } : {}),
      provider: form.provider,
      model_pattern: form.model_pattern.trim(),
      region: form.region.trim() || null,
      input_per_mtok: prices.input_per_mtok as string,
      output_per_mtok: prices.output_per_mtok as string,
      cache_read_per_mtok: prices.cache_read_per_mtok ?? null,
      cache_write_per_mtok: prices.cache_write_per_mtok ?? null,
      notes: form.notes.trim() || null,
    })
  }

  return (
    <form onSubmit={submit} className={styles.form} noValidate>
      {replacing && (
        <p className={styles.formNote}>
          The new price applies to requests from now on. The current row is kept and closed, so
          estimates already made do not change.
        </p>
      )}
      <div className={styles.formRow}>
        {/* Replacing keeps what the row matches; changing that is a new row. */}
        <Select label="Provider" value={form.provider} onChange={set('provider')} disabled={!!replacing}>
          {providers.map((p) => (
            <option key={p} value={p}>{providerLabel(p)}</option>
          ))}
        </Select>
        <Input
          label="Region (optional)"
          value={form.region}
          onChange={set('region')}
          placeholder="Any region"
          className={styles.monoInput}
          disabled={!!replacing}
          spellCheck={false}
        />
      </div>
      <Input
        label="Model pattern"
        value={form.model_pattern}
        onChange={set('model_pattern')}
        placeholder="claude-sonnet-4*"
        error={errors.model_pattern}
        className={styles.monoInput}
        disabled={!!replacing}
        spellCheck={false}
        aria-describedby="price-pattern-hint"
      />
      <p id="price-pattern-hint" className={styles.hint}>
        A glob over model ids: <code>*</code> matches any run of characters.
      </p>
      <fieldset className={styles.priceGrid}>
        <legend className={styles.legend}>Price per million tokens, USD</legend>
        {PRICE_FIELDS.map((f) => (
          <Input
            key={f.key}
            label={f.optional ? `${f.label} (optional)` : f.label}
            value={form[f.key]}
            onChange={set(f.key)}
            inputMode="decimal"
            placeholder={f.optional ? 'None' : '0.00'}
            error={errors[f.key]}
            className={styles.numInput}
          />
        ))}
      </fieldset>
      <Input label="Notes (optional)" value={form.notes} onChange={set('notes')} placeholder="Where this price came from" />
      <div className={styles.formActions}>
        <Button type="button" variant="secondary" onClick={onCancel} disabled={saving}>Cancel</Button>
        <Button type="submit" variant="primary" disabled={saving}>
          {saving ? 'Saving…' : replacing ? 'Replace price' : 'Add price'}
        </Button>
      </div>
    </form>
  )
}

// ---------------------------------------------------------------------------
// PricesTab
// ---------------------------------------------------------------------------

export function PricesTab() {
  const { toast } = useToast()
  const [data, setData] = useState<ModelPricesResult | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [showHistory, setShowHistory] = useState(false)

  /** null = closed; 'new' = Add; a row = Replace that row. */
  const [editing, setEditing] = useState<ModelPrice | 'new' | null>(null)
  const [retiring, setRetiring] = useState<ModelPrice | null>(null)
  const [saving, setSaving] = useState(false)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      setData(await listModelPrices())
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  const now = Date.now()
  const groups = useMemo(() => {
    const at = Date.now()
    const rows = (data?.rows ?? []).filter((r) => showHistory || isInForce(r, at))
    const by = new Map<string, ModelPrice[]>()
    for (const r of rows) {
      const list = by.get(r.provider) ?? []
      list.push(r)
      by.set(r.provider, list)
    }
    for (const list of by.values()) {
      list.sort((a, b) => a.model_pattern.localeCompare(b.model_pattern) || b.effective_from.localeCompare(a.effective_from))
    }
    return [...by.entries()].sort(([a], [b]) => compareProviders(a, b))
  }, [data, showHistory])

  const providers = useMemo(() => {
    const set = new Set([...KNOWN_PROVIDERS, ...(data?.rows ?? []).map((r) => r.provider)])
    return [...set].sort(compareProviders)
  }, [data])

  const historyCount = (data?.rows ?? []).filter((r) => !isInForce(r, now)).length

  async function handleSave(input: ModelPriceInput) {
    setSaving(true)
    try {
      await putModelPrice(input)
      toast(input.id ? 'Price replaced — applies from now on' : 'Price added', 'success')
      setEditing(null)
      await load()
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setSaving(false)
    }
  }

  async function handleRetire() {
    if (!retiring) return
    setSaving(true)
    try {
      await deleteModelPrice(retiring.id)
      toast('Price retired', 'success')
      setRetiring(null)
      await load()
    } catch (err) {
      toast((err as Error).message, 'error')
    } finally {
      setSaving(false)
    }
  }

  const COLS = 10

  return (
    <div>
      <div className={styles.header}>
        <p className={styles.note}>
          {seedNote(data?.seed_generated_at)} Spend on Activity is estimated from these rows; a model
          with no matching row shows as unpriced.
        </p>
        <div className={styles.headerActions}>
          {historyCount > 0 && (
            <label className={styles.toggle}>
              <input type="checkbox" checked={showHistory} onChange={(e) => setShowHistory(e.target.checked)} />
              Show replaced and retired ({historyCount})
            </label>
          )}
          <Button variant="primary" size="sm" onClick={() => setEditing('new')}>Add price</Button>
        </div>
      </div>

      {error ? (
        <div className={styles.errorCard} role="alert">
          <span>Prices could not be loaded: {error}</span>
          <Button variant="secondary" size="sm" onClick={() => void load()}>Try again</Button>
        </div>
      ) : (
        <div className={styles.tableCard}>
          <Table label="Model prices">
            <thead>
              <tr>
                <th>Model pattern</th>
                <th>Region</th>
                <th className={styles.num}>Input</th>
                <th className={styles.num}>Output</th>
                <th className={styles.num}>Cache read</th>
                <th className={styles.num}>Cache write</th>
                <th>Effective</th>
                <th>Source</th>
                <th>Notes</th>
                <th><span className={styles.srOnly}>Actions</span></th>
              </tr>
            </thead>
            {loading ? (
              <tbody>
                {Array.from({ length: 4 }).map((_, i) => (
                  <tr key={i}>
                    {Array.from({ length: COLS }).map((__, j) => (
                      <td key={j}><Skeleton width={`${50 + ((i + j) * 17) % 40}%`} height="14px" /></td>
                    ))}
                  </tr>
                ))}
              </tbody>
            ) : groups.length === 0 ? (
              <tbody>
                <TableEmpty colSpan={COLS}>
                  <div className={styles.empty}>
                    <span className={styles.emptyTitle}>No prices yet</span>
                    <span className={styles.emptyHint}>Every request will show as unpriced until a row matches its model.</span>
                    <Button variant="secondary" size="sm" onClick={() => setEditing('new')}>Add price</Button>
                  </div>
                </TableEmpty>
              </tbody>
            ) : (
              groups.map(([provider, rows]) => (
                <tbody key={provider}>
                  <tr className={styles.groupRow}>
                    <th colSpan={COLS} scope="colgroup">{providerLabel(provider)}</th>
                  </tr>
                  {rows.map((r) => {
                    const live = isInForce(r, now)
                    return (
                      <tr key={r.id} className={live ? undefined : styles.historyRow}>
                        <td><code className={styles.mono}>{r.model_pattern}</code></td>
                        <td>{r.region ? <code className={styles.mono}>{r.region}</code> : <span className={styles.muted}>Any</span>}</td>
                        <td className={styles.num}>{formatPerMtok(r.input_per_mtok) ?? '—'}</td>
                        <td className={styles.num}>{formatPerMtok(r.output_per_mtok) ?? '—'}</td>
                        <td className={styles.num}>{formatPerMtok(r.cache_read_per_mtok) ?? <span className={styles.muted}>—</span>}</td>
                        <td className={styles.num}>{formatPerMtok(r.cache_write_per_mtok) ?? <span className={styles.muted}>—</span>}</td>
                        <td className={styles.effective}>{effectiveText(r)}</td>
                        <td>
                          {r.source === 'seed' ? (
                            <Badge variant="neutral" bordered title="Published list price, shipped with Darkplum. Read-only: replace it to change it.">Seed</Badge>
                          ) : (
                            <Badge variant="info" title="Added by an administrator in this organisation.">Admin</Badge>
                          )}
                        </td>
                        <td className={styles.notes} title={r.notes ?? undefined}>{r.notes || <span className={styles.muted}>—</span>}</td>
                        <td className={styles.actions}>
                          {live && (
                            <>
                              <Button variant="ghost" size="sm" onClick={() => setEditing(r)} aria-label={`Replace price for ${r.model_pattern}`}>
                                Replace
                              </Button>
                              <Button variant="ghost" size="sm" onClick={() => setRetiring(r)} aria-label={`Retire price for ${r.model_pattern}`}>
                                Retire
                              </Button>
                            </>
                          )}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              ))
            )}
          </Table>
        </div>
      )}

      <Modal
        open={editing !== null}
        onClose={() => setEditing(null)}
        dismissible={!saving}
        title={editing && editing !== 'new' ? 'Replace price' : 'Add price'}
        description={
          editing && editing !== 'new'
            ? `${providerLabel(editing.provider)} · ${editing.model_pattern}${editing.region ? ` · ${editing.region}` : ''}`
            : 'A price for models matching a pattern. Applies to requests from now on.'
        }
      >
        {editing !== null && (
          <PriceForm
            key={editing === 'new' ? 'new' : editing.id}
            replacing={editing === 'new' ? null : editing}
            providers={providers}
            saving={saving}
            onCancel={() => setEditing(null)}
            onSubmit={(input) => void handleSave(input)}
          />
        )}
      </Modal>

      <Modal
        open={retiring !== null}
        onClose={() => setRetiring(null)}
        dismissible={!saving}
        title="Retire price"
        description={retiring ? `${providerLabel(retiring.provider)} · ${retiring.model_pattern}` : undefined}
      >
        <p className={styles.formNote}>
          From now on, requests to matching models show as unpriced unless another row matches.
          Estimates already made keep this price.
        </p>
        <div className={styles.formActions}>
          <Button variant="secondary" onClick={() => setRetiring(null)} disabled={saving}>Cancel</Button>
          <Button variant="danger" onClick={() => void handleRetire()} disabled={saving}>
            {saving ? 'Retiring…' : 'Retire price'}
          </Button>
        </div>
      </Modal>
    </div>
  )
}
