export type LogInputImage = {
  index: number
  url: string
  filename: string
  width: number
  height: number
}

// Only our private preview endpoint is fetched with the administrator token.
// Historical logs may have dimensions but no retained reference bytes.
export function normalizeLogInputImages(detail: Record<string, any>): LogInputImage[] {
  const dimensions = detail.request_meta?.image_size?.input_dimensions
  const stored = detail.input_images
  const count = Math.min(100, Math.max(0, Math.floor(Number(detail.request_meta?.image_url_parts) || 0)))
  const entries: any[] = Array.isArray(stored) && stored.length
    ? stored
    : Array.isArray(dimensions) && dimensions.length
      ? dimensions
      : Array.from({ length: count }, (_, index) => ({ index: index + 1 }))
  return entries.map((item, index) => ({
    index: Number.isInteger(item?.index) && item.index > 0 ? item.index : index + 1,
    url: typeof item?.url === 'string' && /^\/api\/logs\/input-images\/[a-f0-9]{64}\.(png|jpeg|webp|gif)$/.test(item.url) ? item.url : '',
    filename: typeof item?.filename === 'string' ? item.filename : '',
    width: Number(item?.width) || 0,
    height: Number(item?.height) || 0,
  }))
}

// Never allow input previews to leak into the result list's recursive fallback.
export function logOutputSource(detail: Record<string, any>): unknown {
  if (Array.isArray(detail.output_images) && detail.output_images.length) return detail.output_images
  if (Array.isArray(detail.image_urls) && detail.image_urls.length) return detail.image_urls
  const { input_images, request_meta, request_shape, ...legacy } = detail
  return legacy
}
