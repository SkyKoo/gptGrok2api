function text(value: unknown): string {
  return typeof value === 'string' ? value.trim() : ''
}

export function parseSessionJsonAccount(rawText: string): Record<string, unknown> {
  if (!rawText.trim()) throw new Error('请先粘贴 Session JSON')
  let parsed: unknown
  try { parsed = JSON.parse(rawText) } catch { throw new Error('Session JSON 格式不正确') }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('Session JSON 格式不正确')
  const source = parsed as Record<string, unknown>
  const token = text(source.accessToken) || text(source.access_token)
  if (!token) throw new Error('Session JSON 中没有找到 accessToken')
  const user = source.user && typeof source.user === 'object' && !Array.isArray(source.user)
    ? source.user as Record<string, unknown> : {}
  for (const [top, nested] of [['email', 'email'], ['user_id', 'id']]) {
    const a = text(source[top]), b = text(user[nested])
    const equal = top === 'email' ? a.toLowerCase() === b.toLowerCase() : a === b
    if (a && b && !equal) throw new Error('Session JSON 账号身份字段不一致')
  }
  return {
    access_token: token,
    email: text(source.email) || text(user.email),
    user_id: text(source.user_id) || text(user.id),
    expired: text(source.expires) || text(source.expired),
  }
}
