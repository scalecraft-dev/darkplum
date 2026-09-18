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
 *   2. No `fetch(` outside src/lib/api.ts and src/context/AuthContext.tsx.
 *      Those two files are the only places allowed to speak HTTP; AuthContext
 *      keeps its session calls, and its paths are in ROUTES like everything
 *      else.
 *   3. No literal request path outside src/lib/routes.ts. A path that is
 *      written twice is a path that can drift.
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

/** Prefixes that identify a request path written as a string literal. */
const PATH_PREFIXES = ['/api/v1/', '/v1/', '/auth/']

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

for (const file of walk(SRC_DIR).sort()) {
  const rel = relative(WEB_ROOT, file).split('\\').join('/')
  const lines = readFileSync(file, 'utf8').split('\n')

  lines.forEach((line, i) => {
    const where = `${rel}:${i + 1}`

    if (!FETCH_ALLOWED.includes(rel) && /(^|[^.\w])fetch\s*\(/.test(line)) {
      strayFetch.push(`${where}  ${line.trim()}`)
    }

    if (!PATH_ALLOWED.includes(rel)) {
      for (const prefix of PATH_PREFIXES) {
        if (line.includes(`'${prefix}`) || line.includes(`"${prefix}`) || line.includes(`\`${prefix}`)) {
          strayPaths.push(`${where}  ${line.trim()}`)
          break
        }
      }
    }
  })
}

fail(
  `Calls to fetch() outside ${FETCH_ALLOWED.join(' and ')}:`,
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
