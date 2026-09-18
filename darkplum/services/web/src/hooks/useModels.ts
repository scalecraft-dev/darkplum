import { useCallback, useEffect, useRef, useState } from 'react'
import { listModels } from '../lib/api'
import type { Model } from '../types'

const FAST_POLL_MS = 10_000
const SLOW_POLL_MS = 60_000

interface UseModelsResult {
  models: Model[]
  /** True only until the first fetch settles. Later fetches keep the list on
   *  screen, so a poll or a retry never replaces a usable page with skeletons. */
  isLoading: boolean
  /** A caller-triggered re-read is in flight, for button feedback only. */
  isRefetching: boolean
  error: string | null
  /**
   * Re-runs the fetch immediately — used by "Try again" and by chat when a
   * routing refusal proves the list is stale. Resolves with the models the
   * fetch returned, or null if it failed, so the caller can act on the fresh
   * list without racing the poll.
   */
  refetch: () => Promise<Model[] | null>
}

export function useModels(): UseModelsResult {
  const [models, setModels] = useState<Model[]>([])
  const [isLoading, setIsLoading] = useState(true)
  const [isRefetching, setIsRefetching] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const intervalRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const hasModelsRef = useRef(false)

  const fetchModels = useCallback(async (): Promise<Model[] | null> => {
    try {
      const result = await listModels()
      setModels(result.data)
      setError(null)
      const hadModels = hasModelsRef.current
      const nowHasModels = result.data.length > 0
      hasModelsRef.current = nowHasModels

      if (nowHasModels && !hadModels && intervalRef.current) {
        clearInterval(intervalRef.current)
        intervalRef.current = setInterval(() => void fetchModels(), SLOW_POLL_MS)
      } else if (!nowHasModels && hadModels && intervalRef.current) {
        clearInterval(intervalRef.current)
        intervalRef.current = setInterval(() => void fetchModels(), FAST_POLL_MS)
      }
      return result.data
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load models')
      return null
    } finally {
      setIsLoading(false)
    }
  }, [])

  useEffect(() => {
    void fetchModels()
    const pollRate = hasModelsRef.current ? SLOW_POLL_MS : FAST_POLL_MS
    intervalRef.current = setInterval(() => void fetchModels(), pollRate)

    const onVisibilityChange = () => {
      if (document.hidden) {
        if (intervalRef.current) {
          clearInterval(intervalRef.current)
          intervalRef.current = null
        }
      } else {
        void fetchModels()
        const rate = hasModelsRef.current ? SLOW_POLL_MS : FAST_POLL_MS
        intervalRef.current = setInterval(() => void fetchModels(), rate)
      }
    }

    document.addEventListener('visibilitychange', onVisibilityChange)
    return () => {
      if (intervalRef.current) clearInterval(intervalRef.current)
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }, [fetchModels])

  const refetch = useCallback(async () => {
    setIsRefetching(true)
    try {
      return await fetchModels()
    } finally {
      setIsRefetching(false)
    }
  }, [fetchModels])

  return { models, isLoading, isRefetching, error, refetch }
}
