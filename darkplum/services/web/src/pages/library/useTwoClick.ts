import { useCallback, useEffect, useRef, useState } from 'react'

/**
 * The app's two-click destructive action: the first click arms the button
 * ("Confirm delete") for three seconds, the second does it. Same timing as
 * resetting the system prompt and revoking a key.
 */
export function useTwoClick(action: () => void): { armed: boolean; click: () => void; disarm: () => void } {
  const [armed, setArmed] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const disarm = useCallback(() => {
    if (timer.current) clearTimeout(timer.current)
    timer.current = null
    setArmed(false)
  }, [])
  useEffect(() => () => { if (timer.current) clearTimeout(timer.current) }, [])
  const click = useCallback(() => {
    if (!armed) {
      setArmed(true)
      timer.current = setTimeout(() => setArmed(false), 3000)
      return
    }
    disarm()
    action()
  }, [armed, action, disarm])
  return { armed, click, disarm }
}
