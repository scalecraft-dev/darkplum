/**
 * Folder paths as the library and the bucket use them: segments joined by
 * '/', '' for a root, never a leading or trailing slash. The rules match the
 * api's (and so the bucket key's): lowercase letters, digits and hyphens, a
 * segment of at most 64 characters, at most 5 deep.
 */
import type { LibraryItem, LibraryKind, LibraryScope } from '../../types'

export const FOLDER_SEGMENT_RE = /^[a-z0-9][a-z0-9-]{0,63}$/
export const FOLDER_MAX_DEPTH = 5

const WINDOWS_RESERVED = new Set([
  'con', 'prn', 'aux', 'nul',
  'com1', 'com2', 'com3', 'com4', 'com5', 'com6', 'com7', 'com8', 'com9',
  'lpt1', 'lpt2', 'lpt3', 'lpt4', 'lpt5', 'lpt6', 'lpt7', 'lpt8', 'lpt9',
])

export function segmentsOf(path: string): string[] {
  return path === '' ? [] : path.split('/')
}

export function joinPath(...parts: string[]): string {
  return parts.filter((p) => p !== '').join('/')
}

export function parentOf(path: string): string {
  const i = path.lastIndexOf('/')
  return i < 0 ? '' : path.slice(0, i)
}

export function nameOf(path: string): string {
  const i = path.lastIndexOf('/')
  return i < 0 ? path : path.slice(i + 1)
}

export function isInside(path: string, folder: string): boolean {
  return folder === '' ? path !== '' : path === folder || path.startsWith(folder + '/')
}

/** A folder name as a person typed it -> a segment, or why it cannot be one. */
export function folderNameProblem(raw: string, depthOfParent: number): string | null {
  const name = raw.trim().toLowerCase()
  if (name === '') return 'Enter a name.'
  if (!FOLDER_SEGMENT_RE.test(name)) return 'Use lowercase letters, numbers and hyphens, starting with a letter or number. Up to 64 characters.'
  if (WINDOWS_RESERVED.has(name)) return `“${name}” is a reserved name on Windows, where exports would not open.`
  if (depthOfParent + 1 > FOLDER_MAX_DEPTH) return `Folders go ${FOLDER_MAX_DEPTH} levels deep. Create it one level up.`
  return null
}

/** What a typed name becomes as a segment: "Q3 Audit" -> "q3-audit". */
export function toSegment(raw: string): string {
  return raw
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 64)
}

export function scopeOf(item: LibraryItem): LibraryScope {
  return item.scope ?? (item.visibility === 'org' ? 'org' : 'private')
}

export function pathOf(item: LibraryItem): string {
  return item.path ?? ''
}

export const SCOPE_LABEL: Record<LibraryScope, string> = { private: 'Private', org: 'Organisation' }

/** The file an item is in the bucket, when the api has not said. */
export function fileNameOf(item: Pick<LibraryItem, 'slug' | 'kind' | 'file_name'>): string {
  if (item.file_name) return item.file_name
  const ext: Record<LibraryKind, string> = { skill: 'skill.md', context: 'context.md', tool: 'tool.json' }
  return `${item.slug}.${ext[item.kind]}`
}

/** "Private / legal / contracts" for a drawer line; "legal / contracts" when
 *  the root is said elsewhere. */
export function displayPath(path: string, scope?: LibraryScope): string {
  const parts = segmentsOf(path)
  const head = scope ? [SCOPE_LABEL[scope]] : []
  return [...head, ...parts].join(' / ') || (scope ? SCOPE_LABEL[scope] : '')
}

/** A folder tree built from what the page has seen: every item's path and
 *  every folder listing read so far, so empty folders show too. */
export interface TreeNode {
  path: string
  name: string
  children: TreeNode[]
}

export function buildTree(paths: Iterable<string>): TreeNode[] {
  const root: TreeNode = { path: '', name: '', children: [] }
  const byPath = new Map<string, TreeNode>([['', root]])
  for (const p of paths) {
    if (p === '') continue
    const segs = segmentsOf(p)
    let cur = root
    let acc = ''
    for (const s of segs) {
      acc = joinPath(acc, s)
      let next = byPath.get(acc)
      if (!next) {
        next = { path: acc, name: s, children: [] }
        byPath.set(acc, next)
        cur.children.push(next)
      }
      cur = next
    }
  }
  const sortRec = (n: TreeNode) => {
    n.children.sort((a, b) => a.name.localeCompare(b.name))
    n.children.forEach(sortRec)
  }
  sortRec(root)
  return root.children
}
