/**
 * API/UI parity check.
 *
 * Run: node --experimental-strip-types scripts/check-api-parity.mts
 *      (pnpm run check:parity)
 *
 * Three things are asserted, all of them structural rather than stylistic:
 *
 *   1. Every route the UI can call is declared in the golden file
 *      (docs/api-routes.json) with surface "ui", and every golden entry marked
 *      surface "ui" is actually called by the UI. Both directions, so a route
 *      that is built and never used fails as loudly as one that is used and
 *      never built.
 *   2. No HTTP call outside src/lib/api.ts and src/context/AuthContext.tsx:
 *      `fetch(` however it is reached (`window.fetch`, `globalThis.fetch`,
 *      aliased through `const f = fetch`), `new XMLHttpRequest`, `sendBeacon(`,
 *      `new EventSource`, `new WebSocket`, and an `axios`/`ky`/`got` import.
 *      Those two files are the only places allowed to speak HTTP; AuthContext
 *      keeps its session calls, and its paths are in ROUTES like everything
 *      else.
 *   3. No literal request path outside src/lib/routes.ts — a string that is,
 *      or starts with, /api/v1, /v1 or /auth, with or without a trailing slash,
 *      so a prefix kept in a constant and joined later is caught too. A path
 *      that is written twice is a path that can drift.
 *
 * Checks 2 and 3 are tripwires against hygiene drift, not a proof: a path
 * assembled from pieces none of which look like a path would slip past them.
 * The guarantee that matters is check 1 together with the Go route-inventory
 * tests: every route the server registers is in the golden, and every golden
 * "ui" route is in ROUTES, so a hidden call could at most reach a route that
 * is already declared and already reachable through ROUTES.
 *
 * Exits 1 with a readable list of what is wrong. No dependencies beyond Node.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join, relative } from 'node:path'
import { ALL_ROUTES } from '../src/lib/routes.ts'

const WEB_ROOT = fileURLToPath(new URL('..', import.meta.url))
const SRC_DIR = join(WEB_ROOT, 'src')
const GOLDEN = fileURLToPath(new URL('../../../docs/api-routes.json', import.meta.url))

/** Files allowed to call fetch. Everything else goes through api.ts. */
const FETCH_ALLOWED = ['src/lib/api.ts', 'src/context/AuthContext.tsx']

/** The only file allowed to contain a literal request path. */
const PATH_ALLOWED = ['src/lib/routes.ts']

/**
 * A string literal that is, or begins, a request path: one of the API roots
 * followed by a slash, the closing quote, or a template-literal placeholder.
 * `'/api/v1'` (no trailing slash, kept in a constant to concatenate later)
 * matches as surely as `'/api/v1/users'`.
 */
const PATH_LITERAL = /(['"`])\/(api\/v1|v1|auth)(?=\/|\1|\$\{)/

/**
 * Anything that speaks HTTP from the browser. `\bfetch\s*\(` matches a bare
 * call and a property access alike (`window.fetch(`); the bare-word `fetch`
 * pattern catches it being aliased or passed along (`const f = fetch`,
 * `call(fetch)`). The `X-Requested-With: XMLHttpRequest` header string in
 * api.ts is a quoted word, not a constructor call, and does not match.
 */
const HTTP_PATTERNS: Array<{ re: RegExp; what: string }> = [
  { re: /\bfetch\s*\(/, what: 'fetch()' },
  { re: /\bfetch\b(?!\s*\()(?!\s*:)/, what: 'a reference to fetch' },
  { re: /\bnew\s+XMLHttpRequest\b/, what: 'XMLHttpRequest' },
  { re: /\bsendBeacon\s*\(/, what: 'navigator.sendBeacon()' },
  { re: /\bnew\s+(EventSource|WebSocket)\b/, what: 'EventSource/WebSocket' },
  { re: /\bfrom\s+['"](axios|ky|got|superagent|node-fetch|cross-fetch|isomorphic-fetch)['"]/, what: 'an HTTP client import' },
  { re: /\brequire\(\s*['"](axios|ky|got|superagent|node-fetch|cross-fetch|isomorphic-fetch)['"]\s*\)/, what: 'an HTTP client require' },
]

interface GoldenRoute {
  service: 'api' | 'gateway'
  method: string
  path: string
  auth: string
  surface: 'ui' | 'machine'
  reason?: string
}

const problems: string[] = []

function fail(section: string, lines: string[]): void {
  if (lines.length === 0) return
  problems.push(`${section}\n${lines.map((l) => `    ${l}`).join('\n')}`)
}

// ---------------------------------------------------------------------------
// 1. Routes
// ---------------------------------------------------------------------------

function key(method: string, path: string): string {
  return `${method.toUpperCase()} ${path}`
}

let golden: GoldenRoute[] | null = null
try {
  golden = JSON.parse(readFileSync(GOLDEN, 'utf8')) as GoldenRoute[]
} catch (err) {
  const code = (err as NodeJS.ErrnoException).code
  if (code === 'ENOENT') {
    console.error(
      `Golden route file not found at ${GOLDEN}.\n` +
        'The API side owns it. Until it exists the route diff cannot run; the fetch\n' +
        'and literal-path checks below still do.',
    )
  } else {
    console.error(`Golden route file could not be read: ${(err as Error).message}`)
    process.exit(1)
  }
}

if (golden) {
  if (!Array.isArray(golden)) {
    console.error('Golden route file is not an array.')
    process.exit(1)
  }

  const malformed: string[] = []
  for (const entry of golden) {
    const where = `${entry?.method ?? '?'} ${entry?.path ?? '?'}`
    if (!entry || typeof entry.path !== 'string' || typeof entry.method !== 'string') {
      malformed.push(`${where} — needs both method and path`)
      continue
    }
    if (entry.surface !== 'ui' && entry.surface !== 'machine') {
      malformed.push(`${where} — surface must be "ui" or "machine"`)
    }
    if (entry.surface === 'machine' && !entry.reason) {
      malformed.push(`${where} — a machine surface needs a reason`)
    }
  }
  fail('Golden file entries are malformed:', malformed)

  const goldenUi = new Set(
    golden.filter((e) => e.surface === 'ui').map((e) => key(e.method, e.path)),
  )
  const goldenAll = new Map(golden.map((e) => [key(e.method, e.path), e]))
  const uiRoutes = new Set(ALL_ROUTES.map((r) => key(r.method, r.path)))

  const notInGolden: string[] = []
  for (const route of [...uiRoutes].sort()) {
    if (goldenUi.has(route)) continue
    const known = goldenAll.get(route)
    notInGolden.push(
      known
        ? `${route} — the golden file marks this surface "${known.surface}", but the UI calls it`
        : `${route} — called from src/lib/routes.ts, absent from the golden file`,
    )
  }
  fail('Routes the UI calls that the golden file does not declare as "ui":', notInGolden)

  const unused: string[] = []
  for (const route of [...goldenUi].sort()) {
    if (!uiRoutes.has(route)) {
      unused.push(`${route} — declared surface "ui", but nothing in ROUTES calls it`)
    }
  }
  fail('Routes the golden file declares as "ui" that the UI never calls:', unused)
}

// ---------------------------------------------------------------------------
// 2 & 3. Source scan
// ---------------------------------------------------------------------------

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name)
    if (statSync(full).isDirectory()) {
      walk(full, out)
    } else if (/\.(ts|tsx)$/.test(name)) {
      out.push(full)
    }
  }
  return out
}

const strayFetch: string[] = []
const strayPaths: string[] = []

/**
 * Blank out comments so prose ("the first fetch settles") is not read as
 * code, keeping every newline so reported line numbers stay right. Block
 * comments go first; a line comment is `//` at the start of a line or after
 * whitespace, which leaves `https://` inside a string alone.
 */
function stripComments(source: string): string {
  return source
    .replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, ' '))
    .replace(/(^|\s)\/\/.*$/gm, '$1')
}

for (const file of walk(SRC_DIR).sort()) {
  const rel = relative(WEB_ROOT, file).split('\\').join('/')
  const lines = stripComments(readFileSync(file, 'utf8')).split('\n')

  lines.forEach((line, i) => {
    const where = `${rel}:${i + 1}`

    if (!FETCH_ALLOWED.includes(rel)) {
      const hit = HTTP_PATTERNS.find((p) => p.re.test(line))
      if (hit) strayFetch.push(`${where}  (${hit.what})  ${line.trim()}`)
    }

    if (!PATH_ALLOWED.includes(rel) && PATH_LITERAL.test(line)) {
      strayPaths.push(`${where}  ${line.trim()}`)
    }
  })
}

fail(
  `HTTP calls outside ${FETCH_ALLOWED.join(' and ')}:`,
  strayFetch,
)
fail(
  `Literal request paths outside ${PATH_ALLOWED.join(' and ')} — put them in ROUTES:`,
  strayPaths,
)

// ---------------------------------------------------------------------------

if (problems.length > 0) {
  console.error('\nAPI parity check failed.\n')
  for (const problem of problems) console.error(`  ${problem}\n`)
  process.exit(1)
}

console.log(
  golden
    ? `API parity OK — ${ALL_ROUTES.length} routes, all matched against the golden file.`
    : `Route table OK — ${ALL_ROUTES.length} routes. Golden file missing, so the diff is unverified.`,
)
