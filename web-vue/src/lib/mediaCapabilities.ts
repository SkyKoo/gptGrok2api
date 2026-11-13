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
export interface MediaSelection {
  quality?: string
  size?: string
  resolution?: string
  seconds?: number
  audio?: boolean
  count?: number
  references?: number
  output_format?: string
}
let capabilities: Record<string, MediaCapability> = {}
export function setMediaCapabilities(value?: Record<string, MediaCapability>) { capabilities = value || {} }
export function mediaCapability(model: string) { return capabilities[model] }
export function mediaVariants(capability: MediaCapability): MediaVariant[] {
  return capability.variants?.length ? capability.variants : [capability]
}
// An omitted field is unconstrained for option discovery; submission supplies defaults.
export function matchingMediaVariants(capability: MediaCapability, request: MediaSelection): MediaVariant[] {
  return mediaVariants(capability).filter(variant =>
    (!request.quality || !variant.qualities?.length || variant.qualities.includes(request.quality)) &&
    (!request.size || !variant.sizes?.length || variant.sizes.includes(request.size)) &&
    (!request.resolution || !variant.resolutions?.length || variant.resolutions.includes(request.resolution)) &&
    (request.seconds === undefined || !variant.durations?.length || variant.durations.includes(request.seconds)) &&
    (request.audio === undefined || ((!request.audio || variant.audio === true) && (!variant.audio_required || request.audio))) &&
    ((request.count ?? 1) <= (variant.max_count ?? Infinity)) &&
    ((request.references ?? 0) <= (variant.max_reference_images ?? Infinity)) &&
    (!request.output_format || !variant.output_formats?.length || variant.output_formats.includes(request.output_format)),
  )
}
export function mediaReferenceLimit(model: string, selection?: MediaSelection): number | undefined {
  const capability = mediaCapability(model)
  if (!capability) return undefined
  if (!selection && capability.max_reference_images !== undefined) return capability.max_reference_images
  const variants = matchingMediaVariants(capability, { ...selection, references: undefined })
  if (!variants.length) return 0
  const limits = variants.map(variant => variant.max_reference_images ?? capability.max_reference_images)
  return limits.some(limit => limit === undefined) ? undefined : Math.max(0, ...limits as number[])
}
export function mediaCombinationError(capability: MediaCapability | undefined, request: MediaSelection) {
  if (!capability || matchingMediaVariants(capability, { ...request, audio: request.audio ?? false }).length) return ''
  if (request.audio && matchingMediaVariants(capability, { ...request, audio: false }).length) return '当前参数组合不支持生成音频，请关闭音频或选择支持音频的参数。'
  if (request.references && matchingMediaVariants(capability, { ...request, references: 0 }).length) return '当前参数组合不支持这些参考图，请调整参数或移除参考图。'
  return '所选模型不支持此参数组合，请调整尺寸、时长、分辨率、数量或参考图。'
}
export function validateMediaCombination(model: string, request: MediaSelection) {
  const error = mediaCombinationError(mediaCapability(model), request)
  if (error) throw new Error(error)
}
