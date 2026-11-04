/**
 * Folder paths as the library and the bucket use them.
 *
 * Run: node --experimental-strip-types scripts/check-library-paths.mts
 *      (pnpm run check:library).
 */
import {
  buildTree,
  displayPath,
  fileNameOf,
  folderNameProblem,
  isInside,
  joinPath,
  nameOf,
  parentOf,
  segmentsOf,
  toSegment,
} from '../src/pages/library/libraryPath.ts'

let failures = 0
function check(name: string, got: unknown, want: unknown) {
  if (JSON.stringify(got) === JSON.stringify(want)) return
  failures++
  console.error(`FAIL ${name}\n  got  ${JSON.stringify(got)}\n  want ${JSON.stringify(want)}`)
}

check('segments of root', segmentsOf(''), [])
check('segments', segmentsOf('legal/contracts'), ['legal', 'contracts'])
check('join skips empties', joinPath('', 'legal', '', 'contracts'), 'legal/contracts')
check('parent', parentOf('legal/contracts'), 'legal')
check('parent of top', parentOf('legal'), '')
check('name', nameOf('legal/contracts'), 'contracts')
check('inside: self', isInside('legal', 'legal'), true)
check('inside: child', isInside('legal/contracts', 'legal'), true)
check('inside: sibling prefix', isInside('legalese', 'legal'), false)
check('inside: root holds everything but root', isInside('x', ''), true)
check('inside: root is not inside root', isInside('', ''), false)

check('toSegment', toSegment('Q3 Audit (draft)'), 'q3-audit-draft')
check('toSegment trims hyphens', toSegment('--legal--'), 'legal')
check('toSegment caps at 64', toSegment('a'.repeat(80)).length, 64)
check('folder name ok', folderNameProblem('legal', 0), null)
check('folder name empty', folderNameProblem('', 0) !== null, true)
check('folder name uppercase is normalised, not refused', folderNameProblem('Legal', 0), null)
check('folder name with spaces refused as a segment', folderNameProblem('a b', 0) !== null, true)
check('folder name dot refused', folderNameProblem('..', 0) !== null, true)
check('folder name slash refused', folderNameProblem('a/b', 0) !== null, true)
check('windows reserved refused', (folderNameProblem('con', 0) ?? '').includes('Windows'), true)
check('depth 5 ok', folderNameProblem('e', 4), null)
check('depth 6 refused', (folderNameProblem('f', 5) ?? '').includes('levels'), true)

check('file name skill', fileNameOf({ slug: 'nda-review', kind: 'skill' }), 'nda-review.skill.md')
check('file name context', fileNameOf({ slug: 'style', kind: 'context' }), 'style.context.md')
check('file name tool', fileNameOf({ slug: 'jira', kind: 'tool' }), 'jira.tool.json')
check('file name from api wins', fileNameOf({ slug: 'x', kind: 'tool', file_name: 'given.tool.json' }), 'given.tool.json')
check('display path', displayPath('legal/contracts', 'private'), 'Private / legal / contracts')
check('display path root', displayPath('', 'org'), 'Organisation')
check('display path no root', displayPath('a/b'), 'a / b')

const tree = buildTree(['legal/contracts', 'legal', 'ops/runbooks/aws', 'legal/contracts'])
check('tree roots', tree.map((n) => n.name), ['legal', 'ops'])
check('tree nested', tree[1].children[0].children[0].path, 'ops/runbooks/aws')
check('tree dedupes', tree[0].children.length, 1)
check('tree ignores root', buildTree(['']).length, 0)

if (failures > 0) {
  console.error(`${failures} check(s) failed`)
  process.exit(1)
}
console.log('library paths OK')
