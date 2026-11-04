import { useState } from 'react'
import type { LibraryScope } from '../../types'
import { FolderGlyph, OrgGlyph, PrivateGlyph } from './LibraryGlyphs'
import { SCOPE_LABEL, isInside, type TreeNode } from './libraryPath'
import styles from '../LibraryPage.module.css'

interface Props {
  trees: Record<LibraryScope, TreeNode[]>
  current: { scope: LibraryScope; path: string }
  onNavigate: (scope: LibraryScope, path: string) => void
}

/**
 * The bucket's shape, as a tree: both roots and every folder the page knows
 * about. Shown beside the folder view on a wide screen; on anything narrower
 * the breadcrumbs carry the same information.
 */
export function LibraryTree({ trees, current, onNavigate }: Props) {
  return (
    <nav className={styles.tree} aria-label="Library folders">
      {(['private', 'org'] as LibraryScope[]).map((scope) => (
        <div key={scope} className={styles.treeRoot}>
          <button
            type="button"
            className={`${styles.treeRow} ${styles.treeRootRow} ${current.scope === scope && current.path === '' ? styles.treeRowActive : ''}`}
            aria-current={current.scope === scope && current.path === '' ? 'page' : undefined}
            onClick={() => onNavigate(scope, '')}
          >
            {scope === 'private' ? <PrivateGlyph size={14} /> : <OrgGlyph size={14} />}
            <span>{SCOPE_LABEL[scope]}</span>
          </button>
          <TreeLevel nodes={trees[scope]} depth={1} scope={scope} current={current} onNavigate={onNavigate} />
        </div>
      ))}
    </nav>
  )
}

function TreeLevel({
  nodes,
  depth,
  scope,
  current,
  onNavigate,
}: {
  nodes: TreeNode[]
  depth: number
  scope: LibraryScope
  current: { scope: LibraryScope; path: string }
  onNavigate: (scope: LibraryScope, path: string) => void
}) {
  if (nodes.length === 0) return null
  return (
    <ul className={styles.treeList}>
      {nodes.map((n) => (
        <TreeItem key={n.path} node={n} depth={depth} scope={scope} current={current} onNavigate={onNavigate} />
      ))}
    </ul>
  )
}

function TreeItem({
  node,
  depth,
  scope,
  current,
  onNavigate,
}: {
  node: TreeNode
  depth: number
  scope: LibraryScope
  current: { scope: LibraryScope; path: string }
  onNavigate: (scope: LibraryScope, path: string) => void
}) {
  const onPath = current.scope === scope && isInside(current.path, node.path)
  const active = current.scope === scope && current.path === node.path
  // A folder on the way to the open one starts open; the person can fold it.
  const [open, setOpen] = useState<boolean | null>(null)
  const isOpen = open ?? onPath
  const hasChildren = node.children.length > 0
  return (
    <li>
      <div className={`${styles.treeRow} ${active ? styles.treeRowActive : ''}`} style={{ paddingLeft: `calc(var(--space-3) + ${depth * 14}px)` }}>
        {hasChildren ? (
          <button
            type="button"
            className={styles.treeToggle}
            aria-label={isOpen ? `Collapse ${node.name}` : `Expand ${node.name}`}
            aria-expanded={isOpen}
            onClick={() => setOpen(!isOpen)}
          >
            <svg className={`${styles.treeChevron} ${isOpen ? styles.treeChevronOpen : ''}`} width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <polyline points="9 18 15 12 9 6" />
            </svg>
          </button>
        ) : (
          <span className={styles.treeToggle} />
        )}
        <button type="button" className={styles.treeName} aria-current={active ? 'page' : undefined} onClick={() => onNavigate(scope, node.path)}>
          <FolderGlyph size={14} open={isOpen && hasChildren} />
          <span>{node.name}</span>
        </button>
      </div>
      {isOpen && <TreeLevel nodes={node.children} depth={depth + 1} scope={scope} current={current} onNavigate={onNavigate} />}
    </li>
  )
}
