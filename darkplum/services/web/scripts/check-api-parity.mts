/**
 * API/UI parity check.
 *
 * Run: node --experimental-strip-types scripts/check-api-parity.mts
 *      (pnpm run check:parity). Uses the project's own `typescript` package
 *      for parsing; no other dependency.
 *
 * Three things are asserted, all of them structural rather than stylistic:
 *
 *   1. Every route the UI can call is declared in the golden file
 *      (docs/api-routes.json) with surface "ui", and every golden entry marked
 *      surface "ui" is in ROUTES. Both directions, so a route that is built
 *      and never used fails as loudly as one that is used and never built.
 *   2. Every ROUTES entry is actually reached from a screen, in a value
 *      position (TypeScript AST, so type-only references do not count):
 *      directly, or via a src/lib function some screen references. ROUTES
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
 * Exits 1 with a readable list of what is wrong.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join, relative } from 'node:path'
import ts from 'typescript'
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
// screen uses it. This is a module graph, not a name search: every file's
// imports are resolved, so a reference to an imported binding becomes a
// reference to `<file>#<export>` — two modules exporting the same name, a
// default export, a string literal that happens to spell a function's name,
// none of these can be confused. Type positions are skipped structurally
// (TypeScript AST), so a name that only appears in a type keeps nothing
// alive; an import nobody uses is tsc's job (noUnusedLocals is on).
//
// Node kinds: an export is `<file>#<name>` ("default" for the default
// export). Its edges are the exports it references, plus `ROUTES.<key>`
// tokens (only for the ROUTES that really is lib/routes.ts's table — a
// shadowing local named ROUTES is not it). A screen's edges are the exports
// it references directly. A route is reached if some screen, or some export
// reachable from a screen, has its `ROUTES.<key>` edge.
//
// Known limits, all in the false-positive direction (a used route reported
// dead, which fails CI loudly rather than hiding anything): `export =`,
// `import('./x').then(m => m.f())` without binding the module to a name,
// and a wrapper passed around under a computed property name.
// ---------------------------------------------------------------------------

function parse(file: string): ts.SourceFile {
  return ts.createSourceFile(file, readFileSync(file, 'utf8'), ts.ScriptTarget.Latest, true,
    file.endsWith('.tsx') ? ts.ScriptKind.TSX : ts.ScriptKind.TS)
}

const parsed = new Map<string, ts.SourceFile>()
function source(file: string): ts.SourceFile {
  let sf = parsed.get(file)
  if (!sf) { sf = parse(file); parsed.set(file, sf) }
  return sf
}

/** Resolve a relative module specifier to a file, or null (a package). */
function resolveModule(fromFile: string, spec: ts.Expression | undefined): string | null {
  if (!spec || !ts.isStringLiteral(spec) || !spec.text.startsWith('.')) return null
  const base = join(dirname(fromFile), spec.text)
  for (const cand of [base, `${base}.ts`, `${base}.tsx`, join(base, 'index.ts'), join(base, 'index.tsx')]) {
    try { if (statSync(cand).isFile()) return cand } catch { /* next */ }
  }
  return null
}

const exportKey = (file: string, name: string) => `${relative(WEB_ROOT, file).split('\\').join('/')}#${name}`

/** What a local binding in one file stands for: a named export of another
 *  file, that file's default export, or the whole namespace of it. */
type Binding = { file: string; name: string } | { file: string; namespace: true }

/** The import bindings of a file: local name -> what it refers to. */
function importBindings(sf: ts.SourceFile): Map<string, Binding> {
  const out = new Map<string, Binding>()
  for (const stmt of sf.statements) {
    if (!ts.isImportDeclaration(stmt) || !stmt.importClause || stmt.importClause.isTypeOnly) continue
    const file = resolveModule(sf.fileName, stmt.moduleSpecifier)
    if (!file) continue
    const clause = stmt.importClause
    if (clause.name) out.set(clause.name.text, { file, name: 'default' })
    const nb = clause.namedBindings
    if (nb && ts.isNamespaceImport(nb)) out.set(nb.name.text, { file, namespace: true })
    if (nb && ts.isNamedImports(nb)) {
      for (const el of nb.elements) {
        if (el.isTypeOnly) continue
        out.set(el.name.text, { file, name: (el.propertyName ?? el.name).text })
      }
    }
  }
  return out
}

/**
 * The edges out of one AST subtree, given the file's import bindings: export
 * keys for every imported binding used by value (through a namespace's
 * `.prop` / `['prop']` access as well), `ROUTES.<key>` tokens, and the bare
 * names of local (non-imported) identifiers so a module-internal helper can
 * be followed to its own body. Type positions contribute nothing.
 */
function edgesOf(root: ts.Node, imported: Map<string, Binding>, into: Set<string>, locals: Set<string>): void {
  // Bindings in scope: the file's imports, plus what this subtree binds
  // itself — `const mod = await import('./x')` (a namespace of x), and
  // `const { f } = ns` / `const { f } = await import('./x')` (f is x#f).
  // Collected in a first pass so use before declaration still resolves.
  const imports = imported
  /** Bindings this subtree declares itself, keyed by the declaring node
   *  (two functions may each bind `mod` to different modules). */
  const local: Array<{ name: string; decl: ts.Node; binding: Binding }> = []
  const dynamicTarget = (init: ts.Expression | undefined): string | null => {
    let e = init
    if (e && ts.isAwaitExpression(e)) e = e.expression
    if (e && ts.isCallExpression(e) && e.expression.kind === ts.SyntaxKind.ImportKeyword && e.arguments.length === 1) {
      return resolveModule(root.getSourceFile().fileName, e.arguments[0])
    }
    return null
  }
  const bind = (node: ts.Node): void => {
    if (ts.isVariableDeclaration(node)) {
      const file = dynamicTarget(node.initializer)
      const nsOf = node.initializer && ts.isIdentifier(node.initializer) ? resolve(node.initializer.text, node.initializer) : undefined
      const nsFile = file ?? (nsOf && 'namespace' in nsOf ? nsOf.file : null)
      if (nsFile && ts.isIdentifier(node.name) && file) local.push({ name: node.name.text, decl: node, binding: { file, namespace: true } })
      if (nsFile && ts.isObjectBindingPattern(node.name)) {
        for (const el of node.name.elements) {
          if (!ts.isIdentifier(el.name)) continue
          const prop = el.propertyName && ts.isIdentifier(el.propertyName) ? el.propertyName.text : el.name.text
          local.push({ name: el.name.text, decl: node, binding: { file: nsFile, name: prop } })
        }
      }
    }
    ts.forEachChild(node, bind)
  }

  /** The names a scope-introducing node declares directly. */
  const declaredNames = (scope: ts.Node): Set<string> => {
    const names = new Set<string>()
    const addPattern = (n: ts.BindingName): void => {
      if (ts.isIdentifier(n)) names.add(n.text)
      else for (const el of n.elements) if (!ts.isOmittedExpression(el)) addPattern(el.name)
    }
    if (ts.isFunctionLike(scope)) {
      for (const p of scope.parameters) addPattern(p.name)
      if (ts.isFunctionExpression(scope) && scope.name) names.add(scope.name.text)
    }
    if (ts.isCatchClause(scope) && scope.variableDeclaration) addPattern(scope.variableDeclaration.name)
    const stmts = ts.isBlock(scope) || ts.isSourceFile(scope) || ts.isModuleBlock(scope) ? scope.statements
      : ts.isCaseClause(scope) || ts.isDefaultClause(scope) ? scope.statements : []
    for (const st of stmts) {
      if (ts.isVariableStatement(st)) for (const d of st.declarationList.declarations) addPattern(d.name)
      else if ((ts.isFunctionDeclaration(st) || ts.isClassDeclaration(st) || ts.isEnumDeclaration(st)) && st.name) names.add(st.name.text)
    }
    if (ts.isForStatement(scope) || ts.isForOfStatement(scope) || ts.isForInStatement(scope)) {
      const init = scope.initializer
      if (init && ts.isVariableDeclarationList(init)) for (const d of init.declarations) addPattern(d.name)
    }
    return names
  }

  /**
   * The binding an identifier at `at` refers to: a local declaration in an
   * enclosing scope wins over the file's imports (so a `const ROUTES = …`
   * inside a function is not the route table), unless that declaration is
   * the very one that produced the binding (a dynamic import or a
   * destructure bound above).
   */
  const resolve = (name: string, at: ts.Node): Binding | undefined => {
    for (let n: ts.Node | undefined = at.parent; n; n = n.parent) {
      if (ts.isSourceFile(n)) break
      if (declaredNames(n).has(name)) {
        // The nearest declaration: one of this subtree's own bindings
        // (declared directly by this scope), or a shadow of everything.
        return local.find((l) => l.name === name && isDirectDecl(l.decl, n))?.binding
      }
    }
    // Top level: the file's imports, or a binding declared at top level here.
    return imports.get(name) ?? local.find((l) => l.name === name && isDirectDecl(l.decl, at.getSourceFile()))?.binding
  }
  /** Whether decl (a VariableDeclaration) is declared directly by scope. */
  const isDirectDecl = (decl: ts.Node, scope: ts.Node): boolean => {
    let n: ts.Node | undefined = decl
    while (n && !ts.isVariableStatement(n) && !ts.isVariableDeclarationList(n)) n = n.parent
    if (n && ts.isVariableDeclarationList(n)) n = n.parent
    return !!n && n.parent === scope
  }

  /** Whether an identifier named ROUTES, at this reference, is the real
   *  table from lib/routes. */
  const routesFile = join(SRC_DIR, 'lib', 'routes.ts')
  const isRoutesTable = (name: string, at: ts.Node): boolean => {
    const b = resolve(name, at)
    if (b && 'name' in b) return b.file === routesFile && b.name === 'ROUTES'
    // Inside lib/routes.ts itself, ROUTES is the top-level declaration.
    if (b || name !== 'ROUTES' || root.getSourceFile().fileName !== routesFile) return false
    for (let n: ts.Node | undefined = at.parent; n; n = n.parent) {
      if (ts.isSourceFile(n)) return true
      if (declaredNames(n).has(name)) return false
    }
    return true
  }

  bind(root)

  const visit = (node: ts.Node): void => {
    if (
      ts.isTypeNode(node) ||
      ts.isImportDeclaration(node) ||
      ts.isImportEqualsDeclaration(node) ||
      ts.isExportDeclaration(node) ||
      ts.isTypeAliasDeclaration(node) ||
      ts.isInterfaceDeclaration(node)
    ) return

    // ROUTES.<key>, ns.<prop>, ns['<prop>'] — resolve through the object.
    if ((ts.isPropertyAccessExpression(node) || ts.isElementAccessExpression(node)) && ts.isIdentifier(node.expression)) {
      const obj = node.expression.text
      const prop = ts.isPropertyAccessExpression(node)
        ? node.name.text
        : ts.isStringLiteral(node.argumentExpression) ? node.argumentExpression.text : null
      if (prop && isRoutesTable(obj, node)) into.add(`ROUTES.${prop}`)
      const b = resolve(obj, node)
      if (b && 'namespace' in b && prop) { into.add(exportKey(b.file, prop)); return }
      // `ns.prop` where ns is a named import may be an `export * as ns`
      // alias; recorded as "<file>#<ns>.<prop>" and expanded once every
      // module's aliases are known (see expandNamespaceAliases).
      if (b && 'name' in b && prop) into.add(`${exportKey(b.file, b.name)}.${prop}`)
    }
    if (ts.isIdentifier(node)) {
      // Skip the property side of `a.b` — only `a` is a binding.
      const parent = node.parent
      const isPropName = parent && ts.isPropertyAccessExpression(parent) && parent.name === node
      const isDeclName = parent && (ts.isFunctionDeclaration(parent) || ts.isVariableDeclaration(parent) || ts.isParameter(parent)) && parent.name === node
      if (!isPropName && !isDeclName) {
        const b = resolve(node.text, node)
        if (b && 'name' in b) into.add(exportKey(b.file, b.name))
        else if (!b) locals.add(node.text)
      }
    }
    ts.forEachChild(node, visit)
  }
  visit(root)
}

/**
 * The export graph of one module: export name -> edges. Local helpers are
 * folded in (an exported function that calls a local function that calls a
 * wrapper reaches the wrapper). Handles `export function/const`, `export
 * default`, later `export { f, g as h }`, `export { a as b } from`,
 * `export { default as x } from`, `export * from` and `export * as ns from`.
 */
const moduleExportsMemo = new Map<string, Map<string, Set<string>>>()
function moduleExports(file: string): Map<string, Set<string>> {
  // Memoised, and the entry is registered before the body runs so a
  // circular `export *` pair sees the (still-filling) map of the other side
  // instead of recursing forever. Names the cycle has not yet produced when
  // it is observed are simply not re-exported through that edge, which
  // cannot hide a route: the module that declares it still exports it.
  const memo = moduleExportsMemo.get(file)
  if (memo) return memo
  const out = new Map<string, Set<string>>()
  moduleExportsMemo.set(file, out)
  const sf = source(file)
  const imports = importBindings(sf)
  const hasMod = (n: ts.Node, k: ts.SyntaxKind) =>
    ts.canHaveModifiers(n) && (ts.getModifiers(n) ?? []).some((m) => m.kind === k)

  // Every top-level declaration with its direct edges and the local names it uses.
  const decls = new Map<string, { edges: Set<string>; uses: Set<string> }>()
  const collect = (name: string, node: ts.Node) => {
    const edges = new Set<string>(); const uses = new Set<string>()
    edgesOf(node, imports, edges, uses)
    decls.set(name, { edges, uses })
  }
  for (const stmt of sf.statements) {
    if (ts.isFunctionDeclaration(stmt) && stmt.name) collect(stmt.name.text, stmt)
    else if (ts.isVariableStatement(stmt)) {
      for (const d of stmt.declarationList.declarations) if (ts.isIdentifier(d.name)) collect(d.name.text, d)
    }
  }
  // Close each declaration over the local helpers it uses.
  const closure = (name: string, seen = new Set<string>()): Set<string> => {
    const out = new Set<string>()
    const d = decls.get(name)
    if (!d || seen.has(name)) return out
    seen.add(name)
    for (const e of d.edges) out.add(e)
    for (const u of d.uses) if (decls.has(u)) for (const e of closure(u, seen)) out.add(e)
    return out
  }

  for (const stmt of sf.statements) {
    if (ts.isFunctionDeclaration(stmt) && stmt.name && hasMod(stmt, ts.SyntaxKind.ExportKeyword)) {
      out.set(hasMod(stmt, ts.SyntaxKind.DefaultKeyword) ? 'default' : stmt.name.text, closure(stmt.name.text))
    } else if (ts.isVariableStatement(stmt) && hasMod(stmt, ts.SyntaxKind.ExportKeyword)) {
      for (const d of stmt.declarationList.declarations) if (ts.isIdentifier(d.name)) out.set(d.name.text, closure(d.name.text))
    } else if (ts.isExportAssignment(stmt) && !stmt.isExportEquals) {
      const expr = stmt.expression
      if (ts.isIdentifier(expr) && decls.has(expr.text)) out.set('default', closure(expr.text))
      else {
        const edges = new Set<string>(); const uses = new Set<string>()
        edgesOf(expr, imports, edges, uses)
        for (const u of uses) for (const e of closure(u)) edges.add(e)
        out.set('default', edges)
      }
    } else if (ts.isExportDeclaration(stmt) && !stmt.isTypeOnly) {
      const target = resolveModule(sf.fileName, stmt.moduleSpecifier)
      if (stmt.exportClause && ts.isNamedExports(stmt.exportClause)) {
        for (const el of stmt.exportClause.elements) {
          if (el.isTypeOnly) continue
          const local = (el.propertyName ?? el.name).text
          if (target) out.set(el.name.text, new Set([exportKey(target, local)]))
          else if (decls.has(local)) out.set(el.name.text, closure(local))
          else {
            const b = imports.get(local)
            if (b && 'name' in b) out.set(el.name.text, new Set([exportKey(b.file, b.name)]))
          }
        }
      } else if (stmt.exportClause && ts.isNamespaceExport(stmt.exportClause) && target) {
        // `export * as ns from './x'`: ns.prop resolves to x#prop. Recorded
        // as a namespace alias for screens that import { ns }.
        namespaceAliases.set(exportKey(file, stmt.exportClause.name.text), target)
      } else if (!stmt.exportClause && target) {
        // `export * from './x'`: resolved after every module is loaded, so
        // order and cycles do not matter (see the wildcard fixed point).
        wildcards.set(file, [...(wildcards.get(file) ?? []), target])
      }
    }
  }
  return out
}

/** `<file>#<ns>` -> the file whose exports ns re-exports (`export * as ns`). */
const namespaceAliases = new Map<string, string>()
/** file -> the modules it `export * from`s. */
const wildcards = new Map<string, string[]>()

/** Rewrites "<file>#<ns>.<prop>" edges through `export * as ns` aliases to
 *  "<target>#<prop>"; drops the ones that are not aliases (ordinary
 *  property access on an imported object). */
function expandNamespaceAliases(edges: Set<string>): void {
  for (const e of [...edges]) {
    const dot = e.indexOf('.', e.lastIndexOf('#'))
    if (dot < 0 || e.startsWith('ROUTES.')) continue
    const target = namespaceAliases.get(e.slice(0, dot))
    if (target) edges.add(exportKey(target, e.slice(dot + 1)))
    edges.delete(e)
  }
}

const libFiles = walk(join(SRC_DIR, 'lib'))

/** Every export of every src/lib module, keyed <file>#<name>, with its edges. */
const graph = new Map<string, Set<string>>()
const perModule = new Map<string, Map<string, Set<string>>>()
for (const f of libFiles) perModule.set(f, moduleExports(f))
// Wildcard fixed point: a module re-exports every name its `export *`
// targets have, under the same name, pointing at the target's export. Loops
// until nothing new appears, so chains and cycles resolve regardless of
// file order.
for (let grew = true; grew; ) {
  grew = false
  for (const [f, targets] of wildcards) {
    const mine = perModule.get(f) ?? new Map<string, Set<string>>()
    perModule.set(f, mine)
    for (const t of targets) {
      const theirs = perModule.get(t) ?? moduleExports(t)
      perModule.set(t, theirs)
      for (const name of theirs.keys()) {
        if (name === 'default' || mine.has(name)) continue
        mine.set(name, new Set([exportKey(t, name)])); grew = true
      }
    }
  }
}
for (const [f, exports] of perModule) for (const [name, edges] of exports) { expandNamespaceAliases(edges); graph.set(exportKey(f, name), edges) }

/** The edges out of every screen (any file outside src/lib). */
const screenEdges = new Set<string>()
for (const f of walk(SRC_DIR)) {
  if (libFiles.includes(f)) continue
  const sf = source(f)
  const imports = importBindings(sf)
  edgesOf(sf, imports, screenEdges, new Set())
}
expandNamespaceAliases(screenEdges)

// Reachable exports: a screen's edges, then everything they point at.
const reachable = new Set<string>()
const frontier = [...screenEdges].filter((e) => graph.has(e))
while (frontier.length > 0) {
  const k = frontier.pop()!
  if (reachable.has(k)) continue
  reachable.add(k)
  for (const e of graph.get(k) ?? []) if (graph.has(e) && !reachable.has(e)) frontier.push(e)
}

const unreferenced: string[] = []
for (const routeKey of Object.keys(ROUTES).sort()) {
  const token = `ROUTES.${routeKey}`
  if (screenEdges.has(token)) continue
  const via = [...graph].filter(([, edges]) => edges.has(token)).map(([k]) => k)
  if (via.some((k) => reachable.has(k))) continue
  const names = [...new Set(via.map((k) => k.slice(k.lastIndexOf('#') + 1)))]
  unreferenced.push(
    via.length === 0
      ? `${token} — no src/lib function uses it and nothing outside src/lib references it`
      : `${token} — used by ${names.join(', ')} but no screen reaches ${names.length === 1 ? 'it' : 'them'}`,
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
