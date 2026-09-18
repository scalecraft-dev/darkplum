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
 *      surface "ui" is in ROUTES. Both directions, so a route that is built
 *      and never used fails as loudly as one that is used and never built.
 *   2. Every ROUTES entry is actually reached from a screen: directly, or via
 *      an api.ts wrapper that some file outside src/lib references. ROUTES
 *      membership alone would let a dead wrapper keep a route green.
 *   3. No HTTP call outside src/lib/api.ts and src/context/AuthContext.tsx:
 *      `fetch(` however it is reached (`window.fetch`, `globalThis.fetch`,
 *      aliased through `const f = fetch`), `new XMLHttpRequest`, `sendBeacon(`,
 *      `new EventSource`, `new WebSocket`, and an `axios`/`ky`/`got` import.
 *      Those two files are the only places allowed to speak HTTP; AuthContext
 *      keeps its session calls, and its paths are in ROUTES like everything
 *      else.
 *   4. No literal request path outside src/lib/routes.ts — a string that is,
 *      or starts with, /api/v1, /v1 or /auth, with or without a trailing slash,
 *      so a prefix kept in a constant and joined later is caught too. A path
 *      that is written twice is a path that can drift.
 *
 * Checks 3 and 4 are tripwires against hygiene drift, not a proof: a path
 * assembled from pieces none of which look like a path would slip past them.
 * The guarantee that matters is checks 1 and 2 together with the Go route-inventory
 * tests: every route the server registers is in the golden, and every golden
 * "ui" route is in ROUTES, so a hidden call could at most reach a route that
 * is already declared and already reachable through ROUTES.
 *
 * Exits 1 with a readable list of what is wrong. No dependencies beyond Node.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { join, relative } from 'node:path'
import { ALL_ROUTES, ROUTES } from '../src/lib/routes.ts'

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
// 3 & 4. Source scan
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
 * code, keeping every newline so reported line numbers stay right. This is
 * a small lexer, not a regex: it tracks string, template and regex-literal
 * state so a `//` or `/*` inside a quoted string is left alone and the code
 * after it on the same line is still scanned. Template `${...}` bodies are
 * treated as code. A `/` starts a regex literal when the previous
 * significant character cannot end an expression — good enough for the
 * source this repo has, and an over-eager regex only ever means a comment
 * survives (a false positive on scan), never that code is hidden.
 */
function stripComments(source: string): string {
  const out: string[] = []
  const n = source.length
  let i = 0
  // What the last significant (non-space, non-comment) character was, to
  // decide whether a `/` opens a regex literal or is a divide.
  let prev = ''
  const regexCanFollow = (ch: string) => ch === '' || '(,=:[!&|?{};+-*%<>~^'.includes(ch)
  const templateDepth: number[] = [] // brace depth at each nested `${`
  let braces = 0

  while (i < n) {
    const ch = source[i]
    const next = source[i + 1]

    if (ch === '/' && next === '/') {
      while (i < n && source[i] !== '\n') i++
      continue
    }
    if (ch === '/' && next === '*') {
      i += 2
      while (i < n && !(source[i] === '*' && source[i + 1] === '/')) {
        out.push(source[i] === '\n' ? '\n' : ' ')
        i++
      }
      i += 2
      continue
    }
    if (ch === "'" || ch === '"') {
      const q = ch
      out.push(ch); i++
      while (i < n && source[i] !== q && source[i] !== '\n') {
        if (source[i] === '\\') { out.push(source[i], source[i + 1] ?? ''); i += 2; continue }
        out.push(source[i]); i++
      }
      if (i < n) { out.push(source[i]); i++ }
      prev = q
      continue
    }
    if (ch === '`') {
      out.push(ch); i++
      while (i < n && source[i] !== '`') {
        if (source[i] === '\\') { out.push(source[i], source[i + 1] ?? ''); i += 2; continue }
        if (source[i] === '$' && source[i + 1] === '{') {
          // Code inside the placeholder: lex it recursively up to the
          // matching brace, then continue the template.
          out.push('${'); i += 2
          let depth = 1
          let start = i
          while (i < n && depth > 0) {
            if (source[i] === '{') depth++
            else if (source[i] === '}') depth--
            if (depth > 0) i++
          }
          out.push(stripComments(source.slice(start, i)))
          if (i < n) { out.push('}'); i++ }
          continue
        }
        out.push(source[i]); i++
      }
      if (i < n) { out.push(source[i]); i++ }
      prev = '`'
      continue
    }
    if (ch === '/' && regexCanFollow(prev)) {
      // Regex literal: copy through to the closing unescaped `/`, honouring
      // character classes.
      out.push(ch); i++
      let inClass = false
      while (i < n && source[i] !== '\n') {
        const c = source[i]
        if (c === '\\') { out.push(c, source[i + 1] ?? ''); i += 2; continue }
        if (c === '[') inClass = true
        else if (c === ']') inClass = false
        out.push(c); i++
        if (c === '/' && !inClass) break
      }
      prev = '/'
      continue
    }
    out.push(ch)
    if (ch === '{') braces++
    if (ch === '}') braces--
    if (!/\s/.test(ch)) prev = ch
    i++
  }
  void templateDepth
  void braces
  return out.join('')
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

// ---------------------------------------------------------------------------
// 2. Call sites
//
// A ROUTES entry only proves a route is declared; this proves something on a
// screen uses it. Each `ROUTES.<key>` must be reached either directly from a
// file outside src/lib (AuthContext does this), or through an exported
// wrapper in src/lib/api.ts that some file outside src/lib references.
// Without this, a wrapper nobody calls keeps a "ui" route green forever.
// ---------------------------------------------------------------------------

/** Exported functions of a lib module: name -> the source of its body. A
 *  re-export (`export { a as b } from './api'`) is an export named b whose
 *  "body" is the name a, so reachability flows through it like a wrapper. */
function exportedFunctions(source: string): Map<string, string> {
  const fnRe = /export\s+(?:async\s+)?function\s+(\w+)\s*\(|export\s+const\s+(\w+)\s*=/g
  const starts: Array<{ name: string; at: number }> = []
  for (let m = fnRe.exec(source); m; m = fnRe.exec(source)) starts.push({ name: m[1] ?? m[2], at: m.index })
  const out = new Map<string, string>()
  starts.forEach(({ name, at }, idx) => {
    out.set(name, source.slice(at, starts[idx + 1]?.at ?? source.length))
  })
  for (const m of source.matchAll(/export\s*\{([^}]*)\}\s*from\s*['"][^'"]+['"]/g)) {
    for (const spec of m[1].split(',')) {
      const [orig, alias] = spec.trim().split(/\s+as\s+/).map((x) => x?.trim())
      if (orig) out.set(alias ?? orig, orig)
    }
  }
  return out
}

/**
 * Drops the places a name can appear without being called: import
 * declarations (an import nobody calls is tsc's business — noUnusedLocals)
 * and type positions (`typeof f`, `import type`). Without this, keeping an
 * import alive with `ReturnType<typeof f>` would count as reaching f's route.
 */
function valueUsesOnly(source: string): string {
  return source
    .replace(/import\s+type\s+[^;]*?from\s*['"][^'"]+['"];?/g, '')
    .replace(/import\s*(?:[\w*\s{},]*?)\s*from\s*['"][^'"]+['"];?/g, '')
    .replace(/\btypeof\s+\w+/g, '')
}

const libFiles = walk(join(SRC_DIR, 'lib'))
const nonLibSource = walk(SRC_DIR)
  .filter((f) => !libFiles.includes(f))
  .map((f) => valueUsesOnly(stripComments(readFileSync(f, 'utf8'))))
  .join('\n')

/** Rewrites `import { a as b }` aliases back to `a` in a lib module's source,
 *  so a body that calls the alias counts as referencing the original. */
function unalias(source: string): string {
  let out = source
  for (const m of source.matchAll(/import\s*(?:type\s*)?\{([^}]*)\}\s*from/g)) {
    for (const spec of m[1].split(',')) {
      const parts = spec.trim().split(/\s+as\s+/)
      if (parts.length === 2) out = out.replace(new RegExp(`\\b${parts[1].trim()}\\b`, 'g'), parts[0].trim())
    }
  }
  return out
}

/** Every exported function in src/lib, keyed by file#name (two modules may
 *  export the same name — conversations.ts wraps api.ts's deleteConversation
 *  under the same name), with what its body references, so a route reached
 *  through a lib helper (api.ts -> conversations.ts -> ChatPage) counts. */
const libExports = new Map<string, { name: string; body: string }>()
for (const f of libFiles) {
  const src = unalias(stripComments(readFileSync(f, 'utf8')))
  for (const [name, body] of exportedFunctions(src)) {
    // Bodies too: a lib helper that only names a wrapper in a type keeps
    // nothing alive.
    libExports.set(`${relative(WEB_ROOT, f)}#${name}`, { name, body: valueUsesOnly(body) })
  }
}
const mentions = (haystack: string, name: string) => new RegExp(`\\b${name}\\b`).test(haystack)

// Reachable lib exports: referenced by name from a screen, or from the body
// of another reachable export in a different module. Fixed point over the
// (small) export set. Same-named exports are conflated, which can only make
// something look reachable, never hide a dead route behind a false alarm.
// Imports and type positions are stripped first (valueUsesOnly), so only a
// value use counts; an import nobody calls is tsc's job (noUnusedLocals is
// on, and `pnpm run build` runs tsc), so the two together cover it.
const reachable = new Set<string>()
for (const [k, e] of libExports) if (mentions(nonLibSource, e.name)) reachable.add(k)
for (let grew = true; grew; ) {
  grew = false
  for (const [k, e] of libExports) {
    if (reachable.has(k)) continue
    for (const r of reachable) {
      if (r.split('#')[0] !== k.split('#')[0] && mentions(libExports.get(r)!.body, e.name)) {
        reachable.add(k); grew = true; break
      }
    }
  }
}

const unreferenced: string[] = []
for (const routeKey of Object.keys(ROUTES).sort()) {
  if (mentions(nonLibSource, `ROUTES\\.${routeKey}`)) continue
  const via = [...libExports].filter(([, e]) => mentions(e.body, `ROUTES\\.${routeKey}`)).map(([k]) => k)
  if (via.some((k) => reachable.has(k))) continue
  const names = via.map((k) => k.split('#')[1])
  unreferenced.push(
    via.length === 0
      ? `ROUTES.${routeKey} — no src/lib function uses it and nothing outside src/lib references it`
      : `ROUTES.${routeKey} — used by ${names.join(', ')} but no screen reaches ${names.length === 1 ? 'it' : 'them'}`,
  )
}
fail(
  'Declared routes no screen uses (wire them, or move them to surface "machine" in the golden file):',
  unreferenced,
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
