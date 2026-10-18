import { useCallback, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { exportLibrary } from '../lib/api'
import { saveBlob } from '../lib/download'
import type { LibraryItem } from '../types'
import { useAuth } from '../context/AuthContext'
import { useToast } from '../components/Toast'
import { Button, Tab, TabList, TabPanel } from '../components/ui'
import { KindGlyph } from '../components/ui/KindGlyph'
import { LibraryList } from './library/LibraryList'
import { ItemDrawer, type DrawerTarget } from './library/ItemDrawer'
import { ImportModal } from './library/ImportModal'
import { ToolHostsCard } from './library/ToolHostsCard'
import { KIND_NOUN, TAB_KIND, errorText, type LibraryTabKey } from './library/libraryText'
import styles from './LibraryPage.module.css'

function tabFromParam(value: string | null): LibraryTabKey {
  return value === 'tools' || value === 'context' ? value : 'skills'
}

const TABS: Array<{ key: LibraryTabKey; label: string }> = [
  { key: 'skills', label: 'Skills' },
  { key: 'tools', label: 'Tools' },
  { key: 'context', label: 'Context' },
]

/**
 * The library: skills, tools and context a person keeps in their
 * organisation's own database, shares with colleagues and attaches to chats.
 * Every signed-in person has one. Administrators get two things more, in
 * place rather than on a tab of their own: the governance actions on other
 * people's shared items, and the allowed tool servers on the Tools tab.
 */
export function LibraryPage() {
  const { user } = useAuth()
  const isAdmin = user?.role === 'admin'
  const { toast } = useToast()

  // The open tab lives in the URL, as on the admin pages: a link to
  // ?tab=tools and the tab on screen cannot disagree.
  const [searchParams, setSearchParams] = useSearchParams()
  const tab = tabFromParam(searchParams.get('tab'))
  const setTab = useCallback(
    (next: LibraryTabKey) => {
      setSearchParams(
        (prev) => {
          const params = new URLSearchParams(prev)
          params.set('tab', next)
          return params
        },
        { replace: true },
      )
    },
    [setSearchParams],
  )
  const kind = TAB_KIND[tab]

  const [drawer, setDrawer] = useState<DrawerTarget | null>(null)
  const [importOpen, setImportOpen] = useState(false)
  const [exporting, setExporting] = useState(false)
  const [refreshKey, setRefreshKey] = useState(0)
  const refresh = useCallback(() => setRefreshKey((k) => k + 1), [])
  const hostsHeading = useRef<HTMLHeadingElement>(null)

  const exportAll = async () => {
    setExporting(true)
    try {
      const { blob, filename } = await exportLibrary()
      saveBlob(blob, filename)
      toast(`Exported your library as ${filename}.`, 'success')
    } catch (err) {
      toast(errorText(err, 'The library could not be exported.'), 'error')
    } finally {
      setExporting(false)
    }
  }

  /** From a refused verify: the drawer has closed; once its focus has gone
   *  back to the row, move it on to the allowed-servers card. */
  const showHosts = useCallback(() => {
    setTab('tools')
    window.setTimeout(() => {
      const el = hostsHeading.current
      if (!el) return
      const reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches
      el.scrollIntoView({ block: 'start', behavior: reduce ? 'auto' : 'smooth' })
      el.focus({ preventScroll: true })
    }, 250)
  }, [setTab])

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <div className={styles.headerText}>
          <h1 className={styles.display}>Library</h1>
          <p className={styles.lede}>
            Skills, tools and context you keep, share with your organisation and attach to chats.
          </p>
        </div>
        <div className={styles.headerActions}>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => void exportAll()}
            disabled={exporting}
            title="Everything you can see, as a zip of .md and .json files"
          >
            {exporting ? 'Exporting…' : 'Export all'}
          </Button>
          <Button variant="secondary" size="sm" onClick={() => setImportOpen(true)}>Import</Button>
          <Button variant="primary" size="sm" onClick={() => setDrawer({ mode: 'new', kind })}>
            New {KIND_NOUN[kind]}
          </Button>
        </div>
      </header>

      <TabList label="Library sections" scope="library">
        {TABS.map((t) => (
          <Tab key={t.key} id={t.key} active={tab === t.key} onClick={() => setTab(t.key)}>
            <KindGlyph kind={TAB_KIND[t.key]} />
            {t.label}
          </Tab>
        ))}
      </TabList>

      {TABS.map((t) => (
        <TabPanel key={t.key} id={t.key} scope="library" active={tab === t.key}>
          {t.key === 'tools' && isAdmin && <ToolHostsCard ref={hostsHeading} refreshKey={refreshKey} />}
          <LibraryList
            kind={TAB_KIND[t.key]}
            active={tab === t.key}
            refreshKey={refreshKey}
            onOpen={(item: LibraryItem) => setDrawer({ mode: 'open', item })}
            onNew={() => setDrawer({ mode: 'new', kind: TAB_KIND[t.key] })}
            onImport={() => setImportOpen(true)}
          />
        </TabPanel>
      ))}

      <ItemDrawer
        target={drawer}
        isAdmin={isAdmin}
        onClose={() => setDrawer(null)}
        onChanged={refresh}
        onOpen={(item) => setDrawer({ mode: 'open', item })}
        onShowHosts={showHosts}
      />

      <ImportModal
        open={importOpen}
        kind={kind}
        onClose={() => setImportOpen(false)}
        onImported={(n) => {
          if (n > 0) refresh()
        }}
      />
    </div>
  )
}
