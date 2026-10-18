import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, getSystemPrompt, updateSystemPrompt } from '../lib/api'
import type { SystemPromptSetting } from '../types'
import { Badge, Button } from '../components/ui'
import { Skeleton } from '../components/ui/Skeleton'
import { useToast } from '../components/Toast'
import styles from './SystemPromptCard.module.css'

/** Admin > Models > Policy: the platform system prompt the webchat proxy
 *  prepends to every turn. Custom text, or the built-in default. */
export function SystemPromptCard() {
  const { toast } = useToast()
  const [setting, setSetting] = useState<SystemPromptSetting | null>(null)
  const [draft, setDraft] = useState('')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  // Reset is two clicks: the first arms "Confirm reset" for three seconds,
  // the same pattern as revoking a key, since it discards custom text.
  const [confirmingReset, setConfirmingReset] = useState(false)
  const resetTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  useEffect(() => () => { if (resetTimer.current) clearTimeout(resetTimer.current) }, [])

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const s = await getSystemPrompt()
      setSetting(s)
      setDraft(s.system_prompt)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoading(false)
    }
  }, [])
  useEffect(() => { void load() }, [load])

  const dirty = setting !== null && draft !== setting.system_prompt
  // Count runes like the server does, not UTF-16 units.
  const chars = [...draft].length
  const onReset = () => {
    if (!confirmingReset) {
      setConfirmingReset(true)
      resetTimer.current = setTimeout(() => setConfirmingReset(false), 3000)
      return
    }
    if (resetTimer.current) clearTimeout(resetTimer.current)
    setConfirmingReset(false)
    void save(null)
  }
  const save = async (value: string | null) => {
    setSaving(true)
    try {
      const s = await updateSystemPrompt(value)
      setSetting(s)
      setDraft(s.system_prompt)
      toast(s.custom ? 'System prompt saved. Chat uses it from the next turn.' : 'System prompt reset to the built-in default.', 'success')
    } catch (err) {
      toast(err instanceof ApiError ? err.message : 'Could not save the system prompt', 'error')
    } finally {
      setSaving(false)
    }
  }

  return (
    <section className={styles.card} aria-labelledby="system-prompt-heading">
      <div className={styles.head}>
        <div>
          <h2 id="system-prompt-heading" className={styles.title}>System prompt</h2>
          <p id="system-prompt-scope" className={styles.subtitle}>
            Sent as the system message on every chat request that doesn't include one: webchat, and
            API-key calls without their own. <code>{'{{current_date}}'}</code> becomes today's date.
          </p>
        </div>
        <div className={styles.headMeta}>
          {setting && (
            <Badge variant={setting.custom ? 'info' : 'neutral'}>
              {setting.custom ? 'Custom' : 'Built-in default'}
            </Badge>
          )}
        </div>
      </div>

      {error ? (
        <div className={styles.errorRow} role="alert">
          <span>The system prompt could not be loaded: {error}</span>
          <Button variant="secondary" size="sm" onClick={() => void load()}>Try again</Button>
        </div>
      ) : loading ? (
        <Skeleton height="160px" />
      ) : (
        <>
          <textarea
            className={styles.editor}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            rows={10}
            aria-labelledby="system-prompt-heading"
            aria-describedby="system-prompt-scope"
            disabled={saving}
          />
          <div className={styles.actions}>
            <span className={`${styles.count} ${chars > 19000 ? styles.countNear : ''}`}>
              {chars.toLocaleString()} / 20,000
            </span>
            {setting?.custom && (
              <Button variant="secondary" size="sm" disabled={saving} onClick={onReset}>
                {confirmingReset ? 'Confirm reset' : 'Reset to default'}
              </Button>
            )}
            <Button variant="primary" size="sm" disabled={saving || !dirty || draft.trim() === '' || chars > 20000} onClick={() => void save(draft)}>
              {saving ? 'Saving…' : 'Save prompt'}
            </Button>
          </div>
        </>
      )}
    </section>
  )
}
