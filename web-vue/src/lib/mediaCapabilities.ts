export interface MediaVariant {
  qualities?: string[]
  sizes?: string[]
  aspect_ratios?: string[]
  resolutions?: string[]
  durations?: number[]
  audio?: boolean
  audio_required?: boolean
  default_audio?: boolean
  max_reference_images?: number
  output_formats?: string[]
  max_count?: number
  mask?: boolean
  transparent?: boolean
}
export interface MediaCapability extends MediaVariant {
  kind: 'image' | 'video'
  variants?: MediaVariant[]
}
let capabilities: Record<string, MediaCapability> = {}
export function setMediaCapabilities(value?: Record<string, MediaCapability>) { capabilities = value || {} }
export function mediaCapability(model: string) { return capabilities[model] }
export function mediaReferenceLimit(model: string): number | undefined { return capabilities[model]?.max_reference_images }
export function validateMediaCombination(model: string, request: { quality?: string; size?: string; resolution?: string; seconds?: number; audio?: boolean; count?: number; references?: number; output_format?: string }) {
  const capability = mediaCapability(model)
  if (!capability) return
  const variants = capability.variants?.length ? capability.variants : [capability]
  const matches = variants.some(variant =>
    (!request.quality || !variant.qualities?.length || variant.qualities.includes(request.quality)) &&
    (!request.size || !variant.sizes?.length || variant.sizes.includes(request.size)) &&
    (!request.resolution || !variant.resolutions?.length || variant.resolutions.includes(request.resolution)) &&
    (request.seconds === undefined || !variant.durations?.length || variant.durations.includes(request.seconds)) &&
    (!request.audio || variant.audio === true) &&
    (!variant.audio_required || request.audio === true) &&
    ((request.count ?? 1) <= (variant.max_count ?? Infinity)) &&
    ((request.references ?? 0) <= (variant.max_reference_images ?? Infinity)) &&
    (!request.output_format || !variant.output_formats?.length || variant.output_formats.includes(request.output_format)),
  )
  if (!matches) throw new Error('所选模型不支持此参数组合，请调整尺寸、时长、分辨率、数量或参考图。')
}
