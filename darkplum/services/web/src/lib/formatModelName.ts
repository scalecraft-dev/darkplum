export function formatModelName(id: string): string {
  let name = id.includes('/') ? id.split('/').pop()! : id

  name = name
    .replace(/-instruct/gi, '')
    .replace(/-chat/gi, '')
    .replace(/-v\d+(\.\d+)*/gi, '')
    .replace(/-hf$/i, '')

  const sizeMatch = name.match(/(\d+)[bB]/i)
  const size = sizeMatch ? sizeMatch[1] + 'B' : null

  const family = name
    .replace(/[-_]\d+[bBx].*/, '')
    .replace(/[-_]/g, ' ')
    .replace(/\b\w/g, (c) => c.toUpperCase())
    .trim()

  return size ? `${family} ${size}` : family
}
