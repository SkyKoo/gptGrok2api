export function isVideoUrl(value: string): boolean {
  try {
    return /\.(mp4|webm|mov|m4v)$/i.test(new URL(value, 'https://local.invalid').pathname)
  } catch {
    return false
  }
}
