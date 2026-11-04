/**
 * Images and files attached to a chat turn: what may be attached, how much,
 * and the shape it takes on the wire.
 *
 * Pure: no DOM, so `scripts/check-attachments.mts` can run it under node.
 * Reading and resizing files lives in attachmentFiles.ts.
 *
 * The limits mirror the gateway's (services/gateway, one set of constants)
 * and Bedrock Converse's per-block caps. The gateway is the authority; these
 * exist so the composer can refuse by name before anything is sent.
 */

export type AttachmentKind = 'image' | 'file'

/** What a person attached to the turn they are writing. Bytes are held as
 *  base64 until the turn is saved; afterwards only the record remains. */
export interface PendingAttachment {
  kind: AttachmentKind
  mediaType: string
  name: string
  /** Decoded size, in bytes. */
  size: number
  /** Base64 of the bytes, as the data: URL will carry them. */
  data: string
  /** An object URL for the composer's thumbnail. Revoked on remove or send. */
  previewUrl?: string
  /** Set when the photo was shrunk to fit; shown in the tile's tooltip. */
  resizedTo?: { width: number; height: number }
}

/** An attachment the server holds, as its metadata comes back. */
export interface AttachmentMeta {
  id: string
  kind: AttachmentKind
  media_type: string
  name: string
  size_bytes: number
}

/** What a message on screen carries: the stored record once saved, the pending
 *  bytes until then. `id` is what tells the two apart. */
export interface MessageAttachment {
  id?: string
  kind: AttachmentKind
  mediaType: string
  name: string
  size: number
  data?: string
  previewUrl?: string
}

export const IMAGE_MAX_BYTES = 3_750_000
export const DOCUMENT_MAX_BYTES = 4_500_000
export const IMAGE_MAX_COUNT = 20
export const DOCUMENT_MAX_COUNT = 5
/** Decoded bytes across every attachment in one request, history included:
 *  base64 inflates it by a third and the whole request must fit in 16 MiB. */
export const TOTAL_MAX_BYTES = 10 * 1024 * 1024
/** Photos are shrunk to this long edge before they are encoded. */
export const IMAGE_MAX_EDGE = 1568

export const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'] as const

export const DOCUMENT_TYPES = [
  'application/pdf',
  'text/csv',
  'application/msword',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  'application/vnd.ms-excel',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  'text/html',
  'text/plain',
  'text/markdown',
] as const

/** Browsers leave File.type empty for .md and .csv more often than not, so the
 *  extension decides when the type is missing or generic. */
const EXT_TYPES: Record<string, string> = {
  png: 'image/png',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  gif: 'image/gif',
  webp: 'image/webp',
  pdf: 'application/pdf',
  csv: 'text/csv',
  doc: 'application/msword',
  docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  xls: 'application/vnd.ms-excel',
  xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  html: 'text/html',
  htm: 'text/html',
  txt: 'text/plain',
  md: 'text/markdown',
  markdown: 'text/markdown',
}

/** The short label a tile shows for a document. */
const TYPE_LABELS: Record<string, string> = {
  'application/pdf': 'PDF',
  'text/csv': 'CSV',
  'application/msword': 'DOC',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document': 'DOCX',
  'application/vnd.ms-excel': 'XLS',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet': 'XLSX',
  'text/html': 'HTML',
  'text/plain': 'TXT',
  'text/markdown': 'MD',
}

/** What the file picker is told to offer. */
export const ACCEPT = [...IMAGE_TYPES, ...DOCUMENT_TYPES, '.md', '.csv', '.txt', '.pdf', '.docx', '.xlsx'].join(',')

export function extensionOf(name: string): string {
  const dot = name.lastIndexOf('.')
  return dot >= 0 ? name.slice(dot + 1).toLowerCase() : ''
}

export type Classified =
  | { ok: true; kind: AttachmentKind; mediaType: string }
  | { ok: false; reason: 'heic' | 'unsupported' }

/** Decides what a file is from its declared type, then its extension. */
export function classify(name: string, declaredType: string): Classified {
  const declared = declaredType.toLowerCase().split(';')[0].trim()
  const ext = extensionOf(name)
  if (declared === 'image/heic' || declared === 'image/heif' || ext === 'heic' || ext === 'heif') {
    return { ok: false, reason: 'heic' }
  }
  const fromDeclared = (IMAGE_TYPES as readonly string[]).includes(declared) || (DOCUMENT_TYPES as readonly string[]).includes(declared)
    ? declared
    : ''
  const mediaType = fromDeclared || EXT_TYPES[ext] || ''
  if (!mediaType) return { ok: false, reason: 'unsupported' }
  const kind: AttachmentKind = (IMAGE_TYPES as readonly string[]).includes(mediaType) ? 'image' : 'file'
  return { ok: true, kind, mediaType }
}

export function typeLabel(mediaType: string): string {
  return TYPE_LABELS[mediaType] ?? mediaType.split('/')[1]?.toUpperCase() ?? 'FILE'
}

/** Binary units, as the limits are counted. Two decimals within a tenth of a
 *  limit, so "3.80 MB" next to a 3.75 MB limit never looks contradictory. */
export function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const kb = bytes / 1024
  if (kb < 1024) return `${Math.round(kb)} KB`
  const mb = kb / 1024
  const nearLimit = [IMAGE_MAX_BYTES, DOCUMENT_MAX_BYTES].some((l) => Math.abs(bytes - l) / l < 0.1)
  return `${mb.toFixed(nearLimit ? 2 : 1)} MB`
}

const SUPPORTED_LIST = 'an image (PNG, JPEG, GIF, WebP) or a PDF, Word, Excel, CSV, HTML, text or Markdown file'

/** Why one more file cannot join the ones already attached, in the words the
 *  composer shows. Null when it can. `history` is what earlier turns already
 *  carry, which every later request re-sends. */
export function checkAdd(
  existing: { kind: AttachmentKind; size: number }[],
  incoming: { name: string; kind: AttachmentKind; mediaType: string; size: number },
  history: { kind: AttachmentKind; size: number }[] = [],
): string | null {
  const { name, kind, size } = incoming
  if (size === 0) return `${name} is empty.`
  const perFile = kind === 'image' ? IMAGE_MAX_BYTES : DOCUMENT_MAX_BYTES
  if (size > perFile) {
    return kind === 'image'
      ? `${name} is ${formatSize(size)}. Images can be up to ${formatSize(IMAGE_MAX_BYTES)}.`
      : `${name} is ${formatSize(size)}. Documents can be up to ${formatSize(DOCUMENT_MAX_BYTES)}.`
  }
  const all = [...history, ...existing]
  const images = all.filter((a) => a.kind === 'image').length
  const docs = all.filter((a) => a.kind === 'file').length
  if (kind === 'image' && images >= IMAGE_MAX_COUNT) {
    return `Up to ${IMAGE_MAX_COUNT} images per conversation. Remove one to add ${name}.`
  }
  if (kind === 'file' && docs >= DOCUMENT_MAX_COUNT) {
    return `Up to ${DOCUMENT_MAX_COUNT} documents per conversation. Remove one to add ${name}.`
  }
  const total = all.reduce((n, a) => n + a.size, 0) + size
  if (total > TOTAL_MAX_BYTES) {
    return `This would make the conversation larger than ${formatSize(TOTAL_MAX_BYTES)} of attachments. Remove a file, or start a new conversation.`
  }
  return null
}

export function unsupportedMessage(name: string, reason: 'heic' | 'unsupported'): string {
  if (reason === 'heic') {
    return `${name} is a HEIC photo, which isn't supported. Export it as JPEG or PNG and add it again.`
  }
  const ext = extensionOf(name)
  return `${name}: ${ext ? `${ext.toUpperCase()} files aren't` : 'this type isn\'t'} supported. Add ${SUPPORTED_LIST}.`
}

// --- wire ---------------------------------------------------------------------

/** OpenAI multi-part content, as the gateway reads it. A stored attachment is
 *  sent as its id (`file_id`), which the api expands from the conversation's
 *  own record; one not yet stored travels as a data: URL. */
export type WirePart =
  | { type: 'text'; text: string }
  | { type: 'image_url'; image_url: { url: string } }
  | { type: 'file'; file: { file_id: string } }
  | { type: 'file'; file: { filename: string; file_data: string } }

export interface WireMessage {
  role: 'user' | 'assistant'
  content: string | WirePart[]
}

export function dataUrl(mediaType: string, base64: string): string {
  return `data:${mediaType};base64,${base64}`
}

/** Thrown when a turn carries an attachment that can no longer be sent: the
 *  record of something the server does not hold and this tab no longer has.
 *  The send is refused by name rather than going out without it. */
export class AttachmentUnavailableError extends Error {
  readonly attachmentName: string
  constructor(attachmentName: string) {
    super(`The attachment "${attachmentName}" is no longer available. Remove it from this message, or add the file again.`)
    this.name = 'AttachmentUnavailableError'
    this.attachmentName = attachmentName
  }
}

export function toWireContent(text: string, attachments: MessageAttachment[] | undefined): string | WirePart[] {
  if (!attachments || attachments.length === 0) return text
  const parts: WirePart[] = []
  if (text) parts.push({ type: 'text', text })
  for (const a of attachments) {
    if (a.id) {
      parts.push({ type: 'file', file: { file_id: a.id } })
    } else if (a.data) {
      if (a.kind === 'image') {
        parts.push({ type: 'image_url', image_url: { url: dataUrl(a.mediaType, a.data) } })
      } else {
        // The filename is informational on this hop; the gateway sends the
        // model a neutral name, never this one.
        parts.push({ type: 'file', file: { filename: a.name, file_data: dataUrl(a.mediaType, a.data) } })
      }
    } else {
      throw new AttachmentUnavailableError(a.name)
    }
  }
  return parts
}

/** Stored metadata -> what a message on screen carries. */
export function fromMeta(m: AttachmentMeta): MessageAttachment {
  return { id: m.id, kind: m.kind, mediaType: m.media_type, name: m.name, size: m.size_bytes }
}

/** A pending attachment -> what a message on screen carries until it is saved. */
export function fromPending(p: PendingAttachment): MessageAttachment {
  return { kind: p.kind, mediaType: p.mediaType, name: p.name, size: p.size, data: p.data, previewUrl: p.previewUrl }
}
