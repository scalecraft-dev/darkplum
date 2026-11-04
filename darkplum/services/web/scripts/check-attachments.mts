/**
 * The attachment rules, under node: what may be attached, the limits, and
 * the content parts a turn becomes on the wire.
 *
 * Run: node --experimental-strip-types scripts/check-attachments.mts
 *      (pnpm run check:attachments).
 */
import {
  AttachmentUnavailableError,
  DOCUMENT_MAX_BYTES,
  DOCUMENT_MAX_COUNT,
  IMAGE_MAX_BYTES,
  IMAGE_MAX_COUNT,
  TOTAL_MAX_BYTES,
  checkAdd,
  classify,
  formatSize,
  toWireContent,
  unsupportedMessage,
} from '../src/lib/attachments.ts'

let failures = 0
function check(name: string, ok: boolean, detail?: unknown) {
  if (ok) return
  failures++
  console.error(`FAIL ${name}`, detail === undefined ? '' : JSON.stringify(detail))
}

// --- classify -----------------------------------------------------------
check('png by type', JSON.stringify(classify('a.png', 'image/png')) === JSON.stringify({ ok: true, kind: 'image', mediaType: 'image/png' }))
check('md by extension, empty type', JSON.stringify(classify('notes.md', '')) === JSON.stringify({ ok: true, kind: 'file', mediaType: 'text/markdown' }))
check('csv with generic type', JSON.stringify(classify('x.csv', 'application/octet-stream')) === JSON.stringify({ ok: true, kind: 'file', mediaType: 'text/csv' }))
check('jpeg with charset suffix', classify('p.jpg', 'image/jpeg; charset=binary').ok)
check('heic refused by name', JSON.stringify(classify('IMG_1.HEIC', '')) === JSON.stringify({ ok: false, reason: 'heic' }))
check('svg refused', JSON.stringify(classify('logo.svg', 'image/svg+xml')) === JSON.stringify({ ok: false, reason: 'unsupported' }))
check('zip refused', classify('a.zip', 'application/zip').ok === false)
check('unsupported copy names the extension', unsupportedMessage('a.zip', 'unsupported').startsWith('a.zip: ZIP files aren'))
check('heic copy', unsupportedMessage('x.heic', 'heic').includes('HEIC'))

// --- limits -------------------------------------------------------------
const img = (size: number) => ({ name: 'p.png', kind: 'image' as const, mediaType: 'image/png', size })
const doc = (size: number) => ({ name: 'd.pdf', kind: 'file' as const, mediaType: 'application/pdf', size })
check('empty file', checkAdd([], img(0)) === 'p.png is empty.')
check('image at limit ok', checkAdd([], img(IMAGE_MAX_BYTES)) === null)
check('image over limit', (checkAdd([], img(IMAGE_MAX_BYTES + 1)) ?? '').includes('Images can be up to'))
check('document at limit ok', checkAdd([], doc(DOCUMENT_MAX_BYTES)) === null)
check('document over limit', (checkAdd([], doc(DOCUMENT_MAX_BYTES + 1)) ?? '').includes('Documents can be up to'))
const twentyImages = Array.from({ length: IMAGE_MAX_COUNT }, () => ({ kind: 'image' as const, size: 10 }))
check('20 images then one more', (checkAdd(twentyImages, img(10)) ?? '').startsWith(`Up to ${IMAGE_MAX_COUNT} images`))
check('19 images then one more ok', checkAdd(twentyImages.slice(1), img(10)) === null)
const fiveDocs = Array.from({ length: DOCUMENT_MAX_COUNT }, () => ({ kind: 'file' as const, size: 10 }))
check('5 documents then one more', (checkAdd(fiveDocs, doc(10)) ?? '').startsWith(`Up to ${DOCUMENT_MAX_COUNT} documents`))
check('history counts toward the limit', (checkAdd([], img(10), twentyImages) ?? '').startsWith('Up to'))
const nearTotal = [{ kind: 'file' as const, size: TOTAL_MAX_BYTES - 100 }]
check('total over limit', (checkAdd(nearTotal, img(101)) ?? '').includes('larger than'))
check('total at limit ok', checkAdd(nearTotal, img(100)) === null)
check('total counts history', (checkAdd([], img(101), nearTotal) ?? '').includes('larger than'))

// --- size format --------------------------------------------------------
check('bytes', formatSize(900) === '900 B')
check('kb', formatSize(842 * 1024) === '842 KB')
check('mb one decimal', formatSize(1.2 * 1024 * 1024) === '1.2 MB')
check('mb two decimals near a limit', formatSize(IMAGE_MAX_BYTES) === '3.58 MB')

// --- wire ---------------------------------------------------------------
check('text only stays a string', toWireContent('hi', undefined) === 'hi')
check('text with empty list stays a string', toWireContent('hi', []) === 'hi')
const stored = { id: 'a1', kind: 'image' as const, mediaType: 'image/png', name: 'p.png', size: 3 }
const pendingImage = { kind: 'image' as const, mediaType: 'image/png', name: 'q.png', size: 3, data: 'QUJD' }
const pendingDoc = { kind: 'file' as const, mediaType: 'application/pdf', name: 'd.pdf', size: 3, data: 'QUJD' }
const gone = { kind: 'file' as const, mediaType: 'application/pdf', name: 'old.pdf', size: 3 }
const parts = toWireContent('look', [stored, pendingImage, pendingDoc])
check('parts: text first then one per attachment', Array.isArray(parts) && parts.length === 4, parts)
let threw: unknown = null
try {
  toWireContent('look', [stored, gone])
} catch (e) {
  threw = e
}
check('an attachment with no id and no bytes refuses the send by name', threw instanceof AttachmentUnavailableError && (threw as Error).message.includes('old.pdf'), String(threw))
if (Array.isArray(parts)) {
  check('stored goes as a reference', JSON.stringify(parts[1]) === JSON.stringify({ type: 'file', file: { file_id: 'a1' } }), parts[1])
  check('pending image goes as a data URL', JSON.stringify(parts[2]) === JSON.stringify({ type: 'image_url', image_url: { url: 'data:image/png;base64,QUJD' } }), parts[2])
  check('pending document goes as file_data', JSON.stringify(parts[3]) === JSON.stringify({ type: 'file', file: { filename: 'd.pdf', file_data: 'data:application/pdf;base64,QUJD' } }), parts[3])
}
const noText = toWireContent('', [stored])
check('image-only turn has no empty text part', Array.isArray(noText) && noText.length === 1 && noText[0].type === 'file')

if (failures > 0) {
  console.error(`${failures} check(s) failed`)
  process.exit(1)
}
console.log('attachments OK')
