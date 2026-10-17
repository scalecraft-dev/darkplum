import { createContext, useContext, useRef } from 'react'
import styles from './Tabs.module.css'

// ---------------------------------------------------------------------------
// The WAI-ARIA tab pattern, implemented once for every page that uses tabs.
//
// A tab and its panel are tied together by ids derived from the same `id` a
// caller already has for its tab key, so no caller has to invent a pair:
//
//   <TabList label="Models sections">
//     <Tab id="models" active={tab === 'models'} onClick={…}>Models</Tab>
//   </TabList>
//   <TabPanel id="models" active={tab === 'models'}>…</TabPanel>
//
// Selection follows focus (automatic activation), which is what the pattern
// calls for when switching tabs is cheap and has no side effects — as it is
// here, where every tab's data is already in hand. Panels are hidden, not
// unmounted, so switching away never discards what someone was doing.
// ---------------------------------------------------------------------------

/** Two tab sets on one page must never share an id. A TabList's `scope`
 *  prefixes every id under it; a TabPanel is a sibling of the list, so it
 *  takes the same `scope` explicitly. */
const tabDomId = (scope: string | undefined, id: string) => `tab-${scope ? `${scope}-` : ''}${id}`
const panelDomId = (scope: string | undefined, id: string) => `tabpanel-${scope ? `${scope}-` : ''}${id}`
const ScopeContext = createContext<string | undefined>(undefined)

// ---------------------------------------------------------------------------
// TabList — the container row, and the keyboard contract
// ---------------------------------------------------------------------------

interface TabListProps {
  /** Names the set for anyone who reaches it by keyboard or screen reader —
   *  "Models sections" rather than a second, unexplained list of buttons. */
  label: string
  /** Id prefix shared with this list's TabPanels (see tabDomId). */
  scope?: string
  children: React.ReactNode
}

export function TabList({ label, scope, children }: TabListProps) {
  const ref = useRef<HTMLDivElement>(null)

  /** Left/Right wrap around the row, Home/End jump to its ends. The tabs are
   *  read from the DOM rather than from a registry, so the order a reader
   *  moves through is exactly the order they are rendered in. Focus moves and
   *  the tab is activated, as automatic activation requires. */
  const onKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    const keys = ['ArrowLeft', 'ArrowRight', 'Home', 'End']
    if (!keys.includes(e.key)) return
    const el = ref.current
    if (!el) return
    const tabs = [...el.querySelectorAll<HTMLButtonElement>('[role="tab"]:not([disabled])')]
    if (tabs.length === 0) return
    const current = tabs.findIndex((t) => t === document.activeElement)
    if (current === -1) return
    e.preventDefault()
    const next =
      e.key === 'ArrowLeft'
        ? (current - 1 + tabs.length) % tabs.length
        : e.key === 'ArrowRight'
          ? (current + 1) % tabs.length
          : e.key === 'Home'
            ? 0
            : tabs.length - 1
    tabs[next].focus()
    tabs[next].click()
  }

  return (
    <ScopeContext.Provider value={scope}>
      <div
        ref={ref}
        className={styles.tabList}
        role="tablist"
        aria-label={label}
        onKeyDown={onKeyDown}
      >
        {children}
      </div>
    </ScopeContext.Provider>
  )
}

// ---------------------------------------------------------------------------
// Tab — a single tab button
// ---------------------------------------------------------------------------

interface TabProps extends Omit<React.ButtonHTMLAttributes<HTMLButtonElement>, 'id'> {
  /** The tab's key. Also what its panel is found by. */
  id: string
  active?: boolean
  count?: number
  children: React.ReactNode
}

export function Tab({ id, active = false, count, children, ...props }: TabProps) {
  const scope = useContext(ScopeContext)
  return (
    <button
      role="tab"
      id={tabDomId(scope, id)}
      aria-selected={active}
      aria-controls={panelDomId(scope, id)}
      // Roving tabindex: Tab reaches the row, then the arrow keys move within
      // it — one stop for the whole set rather than one per tab.
      tabIndex={active ? 0 : -1}
      className={`${styles.tab} ${active ? styles.tabActive : ''}`}
      {...props}
    >
      {children}
      {count !== undefined && (
        <span className={styles.tabCount}>{count}</span>
      )}
    </button>
  )
}

// ---------------------------------------------------------------------------
// TabPanel — what the open tab is naming
// ---------------------------------------------------------------------------

interface TabPanelProps {
  /** The id of the tab this panel belongs to. */
  id: string
  /** The same `scope` as the TabList the tab lives in. */
  scope?: string
  active: boolean
  children: React.ReactNode
}

export function TabPanel({ id, scope, active, children }: TabPanelProps) {
  // Every panel stays mounted and the inactive ones are hidden, rather than
  // unmounted: a half-edited policy or a collapsed provider section must
  // still be there when the person comes back to the tab. `hidden` removes the panel from layout and from the
  // accessibility tree, so nothing leaks; aria-controls can name every
  // panel because every panel exists. One seam to know: a modal opened from
  // inside a panel is portalled to <body> and keeps its own open state, so
  // it is not hidden with the panel. Today every way to switch tabs is a
  // click or a key the modal's focus trap already blocks; a future non-modal
  // entry point (a global shortcut, say) would need to close such a modal
  // first. Links to another page (Admin > Routing, say) are ordinary router
  // links and leave the tab set behind entirely; for the same reason they
  // never appear inside an open modal — the vendor install and enable modals
  // carry none.
  return (
    <div
      role="tabpanel"
      id={panelDomId(scope, id)}
      aria-labelledby={tabDomId(scope, id)}
      hidden={!active}
      // Every panel here opens on prose — a heading, or the note about the
      // organisation's policy — before it reaches a control. The pattern asks
      // for a tab stop in exactly that case, so that tabbing out of the tab
      // row lands on the panel and reads it, rather than skipping past it into
      // the first input.
      tabIndex={0}
      className={styles.tabPanel}
    >
      {children}
    </div>
  )
}
