import { useCallback, useEffect, useState } from 'react'
import {
  deleteModelAlias,
  getRoutingPolicy,
  listAdminModels,
  listModelAliases,
  putModelAlias,
} from '../lib/api'
import { DEFAULT_RESIDENCY } from '../lib/policy'
import type { AdminModel, ModelAlias, ModelAliasInput, Residency, RoutingPolicy } from '../types'
import { useToast } from '../components/Toast'
import { AliasesSection } from './AliasesSection'
import pageStyles from './PageLayout.module.css'

/**
 * Admin > Routing: aliases and routed aliases — the stable names people pick
 * in chat, the routes behind them, and the classifier that picks a route.
 *
 * It reads three things and writes one. The catalogue is what a route can
 * point at; the organisation's policy is what every route's health is judged
 * against; the aliases are what this page edits. All three are read here
 * rather than borrowed from Admin > Models, so this page is correct when it is
 * the first one opened (a link from a refused chat message lands here).
 */
export function RoutingPage() {
  const { toast } = useToast()

  const [aliases, setAliases] = useState<ModelAlias[]>([])
  const [aliasesLoading, setAliasesLoading] = useState(true)
  const [aliasesError, setAliasesError] = useState<string | null>(null)

  const [models, setModels] = useState<AdminModel[]>([])
  const [modelsLoading, setModelsLoading] = useState(true)
  const [modelsError, setModelsError] = useState<string | null>(null)

  const [policy, setPolicy] = useState<RoutingPolicy | null>(null)
  const [policyLoading, setPolicyLoading] = useState(true)
  const [policyError, setPolicyError] = useState<string | null>(null)

  /**
   * The rung every route's health is judged against. Until the policy has
   * loaded there is no answer, and the fallback must not be the loosest rung —
   * that would paint a vendor route as active for the first frame. The api's
   * own default is the safe assumption.
   */
  const residency: Residency = policy?.residency ?? DEFAULT_RESIDENCY

  const loadAliases = useCallback(async () => {
    setAliasesLoading(true)
    try {
      setAliases(await listModelAliases())
      setAliasesError(null)
    } catch (err) {
      setAliasesError((err as Error).message)
    } finally {
      setAliasesLoading(false)
    }
  }, [])

  const loadModels = useCallback(async () => {
    setModelsLoading(true)
    try {
      setModels((await listAdminModels()).models)
      setModelsError(null)
    } catch (err) {
      setModelsError((err as Error).message)
    } finally {
      setModelsLoading(false)
    }
  }, [])

  const loadPolicy = useCallback(async () => {
    setPolicyLoading(true)
    try {
      setPolicy(await getRoutingPolicy())
      setPolicyError(null)
    } catch (err) {
      setPolicyError((err as Error).message)
    } finally {
      setPolicyLoading(false)
    }
  }, [])

  const loadAll = useCallback(() => {
    void loadAliases()
    void loadModels()
    void loadPolicy()
  }, [loadAliases, loadModels, loadPolicy])

  useEffect(() => {
    loadAll()
  }, [loadAll])

  // Throws on failure so the editor can attach the server's message to a field.
  // Resolves with the API's configure-time warnings for the saved alias.
  const handleSave = async (alias: string, body: ModelAliasInput): Promise<string[]> => {
    const existing = aliases.some((a) => a.alias === alias)
    const saved = await putModelAlias(alias, body)
    setAliases((prev) => {
      const next = prev.filter((a) => a.alias !== saved.alias)
      return [...next, saved].sort((a, b) => a.alias.localeCompare(b.alias))
    })
    const changed =
      'kind' in body
        ? `now has ${body.routes.length} ${body.routes.length === 1 ? 'route' : 'routes'}`
        : `now points at ${body.target_model_id}`
    toast(existing ? `Alias ${alias} ${changed}` : `Alias ${alias} created`, 'success')
    return saved.warnings ?? []
  }

  const handleDelete = async (alias: string) => {
    await deleteModelAlias(alias)
    setAliases((prev) => prev.filter((a) => a.alias !== alias))
  }

  /**
   * One failure line for the section, naming which read failed. Every row's
   * status is a judgement made from all three, so a missing catalogue or
   * policy is a reason not to show statuses at all — not a reason to show
   * ones computed against nothing.
   */
  const error = aliasesError
    ? aliasesError
    : modelsError
      ? `The model catalogue could not be read, so route targets cannot be checked: ${modelsError}`
      : policyError
        ? `The routing policy could not be read, so route health cannot be judged: ${policyError}`
        : null

  return (
    <div className={pageStyles.page}>
      <div className={pageStyles.header}>
        <h1 className={pageStyles.title}>Routing</h1>
        <p className={pageStyles.subtitle}>
          Stable model names, the routes behind them, and the classifier that chooses between them
        </p>
      </div>

      <AliasesSection
        aliases={aliases}
        models={models}
        loading={aliasesLoading || modelsLoading || policyLoading}
        error={error}
        residency={residency}
        onRetry={loadAll}
        onSave={handleSave}
        onDelete={handleDelete}
      />
    </div>
  )
}
