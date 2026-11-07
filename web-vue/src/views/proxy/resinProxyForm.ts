export type ResinProxyFields = { address: string; platform: string; token: string }

export function readResinProxyURL(raw: string): ResinProxyFields {
  try {
    const url = new URL(raw)
    const platform = decodeURIComponent(url.username)
    const token = decodeURIComponent(url.password)
    url.username = ''
    url.password = ''
    return { address: url.toString().replace(/\/$/, ''), platform, token }
  } catch {
    return { address: raw, platform: '', token: '' }
  }
}

export function buildResinProxyURL(fields: ResinProxyFields): string {
  if (!fields.address.trim()) return ''
  try {
    const url = new URL(fields.address.trim())
    url.username = encodeURIComponent(fields.platform)
    url.password = encodeURIComponent(fields.token)
    // Preserve the stored URL representation used by existing task bindings.
    // The backend removes the root path only from the dispatched proxy URL.
    return url.toString()
  } catch {
    return fields.address.trim()
  }
}
