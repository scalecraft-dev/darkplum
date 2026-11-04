import { createContext, useContext } from 'react'

/**
 * Where a page may put its own rows in the sidebar.
 *
 * The chat page owns the conversation list (its state, its loading, its
 * renames and deletes) and renders it through a portal into this element,
 * which the Layout places under the Recents heading. The page keeps every
 * piece of state it already had; only the DOM position moves. The element is
 * null until the sidebar has mounted, and on a page that has no slot.
 */
export const SidebarSlotContext = createContext<HTMLElement | null>(null)

export function useSidebarSlot(): HTMLElement | null {
  return useContext(SidebarSlotContext)
}
