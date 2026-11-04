/**
 * Turning a File from the picker, a drop or the clipboard into a
 * PendingAttachment: read it, shrink a photo that would not fit, encode it.
 * Browser-only; the pure rules live in attachments.ts.
 */
import {
  IMAGE_MAX_BYTES,
  IMAGE_MAX_EDGE,
  classify,
  unsupportedMessage,
  type PendingAttachment,
} from './attachments'

export type Prepared = { ok: true; attachment: PendingAttachment } | { ok: false; name: string; error: string }

function readAsBase64(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader()
    r.onerror = () => reject(r.error ?? new Error('read failed'))
    r.onload = () => {
      const s = String(r.result)
      resolve(s.slice(s.indexOf(',') + 1))
    }
    r.readAsDataURL(blob)
  })
}

function loadImage(url: string): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const img = new Image()
    img.onload = () => resolve(img)
    img.onerror = () => reject(new Error('decode failed'))
    img.src = url
  })
}

/** Shrinks a PNG, JPEG or WebP whose long edge is over the limit, or whose
 *  bytes are, to IMAGE_MAX_EDGE. GIFs are left alone (a resize would drop the
 *  animation). Returns null when nothing was changed. */
async function shrink(
  file: File,
  mediaType: string,
): Promise<{ blob: Blob; mediaType: string; width: number; height: number } | null> {
  if (mediaType === 'image/gif') return null
  const url = URL.createObjectURL(file)
  try {
    const img = await loadImage(url)
    const long = Math.max(img.naturalWidth, img.naturalHeight)
    if (long <= IMAGE_MAX_EDGE && file.size <= IMAGE_MAX_BYTES) return null
    const scale = Math.min(1, IMAGE_MAX_EDGE / long)
    const width = Math.max(1, Math.round(img.naturalWidth * scale))
    const height = Math.max(1, Math.round(img.naturalHeight * scale))
    const canvas = document.createElement('canvas')
    canvas.width = width
    canvas.height = height
    const ctx = canvas.getContext('2d')
    if (!ctx) return null
    ctx.drawImage(img, 0, 0, width, height)
    // PNG stays PNG so screenshots keep crisp text, WebP stays WebP so its
    // transparency survives, JPEG stays JPEG. Where the browser cannot encode
    // the type (older Safari and WebP), PNG is the lossless fallback.
    const out = mediaType === 'image/jpeg' ? 'image/jpeg' : mediaType
    let blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, out, 0.85))
    let outType = out
    if ((!blob || blob.type !== out) && out !== 'image/png') {
      blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/png'))
      outType = 'image/png'
    }
    if (!blob) return null
    return { blob, mediaType: outType, width, height }
  } finally {
    URL.revokeObjectURL(url)
  }
}

/** Reads one file into a PendingAttachment, or says why it cannot be. Size
 *  and count checks against what is already attached are the caller's
 *  (checkAdd), because they depend on the other files. */
export async function prepareFile(file: File): Promise<Prepared> {
  const c = classify(file.name, file.type)
  if (!c.ok) return { ok: false, name: file.name, error: unsupportedMessage(file.name, c.reason) }
  try {
    let blob: Blob = file
    let mediaType = c.mediaType
    let name = file.name
    let resizedTo: PendingAttachment['resizedTo']
    if (c.kind === 'image') {
      const shrunk = await shrink(file, mediaType).catch(() => null)
      if (shrunk) {
        blob = shrunk.blob
        if (shrunk.mediaType !== mediaType) {
          mediaType = shrunk.mediaType
          name = name.replace(/\.[^.]+$/, '') + (mediaType === 'image/png' ? '.png' : '.jpg')
        }
        resizedTo = { width: shrunk.width, height: shrunk.height }
      }
    }
    const data = await readAsBase64(blob)
    const attachment: PendingAttachment = {
      kind: c.kind,
      mediaType,
      name,
      size: blob.size,
      data,
      resizedTo,
    }
    if (c.kind === 'image') attachment.previewUrl = URL.createObjectURL(blob)
    return { ok: true, attachment }
  } catch {
    return { ok: false, name: file.name, error: `${file.name} couldn't be read. Remove it and add it again.` }
  }
}

export function releasePreview(a: { previewUrl?: string }): void {
  if (a.previewUrl) URL.revokeObjectURL(a.previewUrl)
}

/** Names for images pasted from the clipboard, which arrive as "image.png". */
export function pastedName(index: number, mediaType: string): string {
  const ext = mediaType === 'image/jpeg' ? 'jpg' : mediaType.split('/')[1] ?? 'png'
  return index === 1 ? `Pasted image.${ext}` : `Pasted image ${index}.${ext}`
}
