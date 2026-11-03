export const GROK_IMAGE_REFERENCE_LIMIT = 5
export const GROK_VIDEO_REFERENCE_LIMIT = 7

export function studioReferenceLimit(mode: string, imageModel: string) {
  if (mode === 'video') return GROK_VIDEO_REFERENCE_LIMIT
  if (mode === 'image' && imageModel.startsWith('grok-')) return GROK_IMAGE_REFERENCE_LIMIT
  return 8
}

export function studioReferenceCountError(mode: string, imageModel: string, count: number) {
  const limit = studioReferenceLimit(mode, imageModel)
  if (count <= limit) return ''
  const label = mode === 'video' ? 'Grok 视频' : mode === 'image' && imageModel.startsWith('grok-') ? 'Grok 生图' : '当前模式'
  return `${label}最多支持 ${limit} 张参考图，当前 ${count} 张，请减少参考图数量`
}

export function validateStudioReferenceCount(mode: string, imageModel: string, count: number) {
  const error = studioReferenceCountError(mode, imageModel, count)
  if (error) throw new Error(error)
}

// Keep endpoint selection and model selection consistent, including saved selections.
export function resolveStudioImageModel(current: string, hasReferences: boolean, available: string[]) {
  if (['grok-imagine-image', 'grok-imagine-image-lite'].includes(current)) current = 'grok-imagine-image-pro'
  const candidates = available.filter(model => hasReferences
    ? !model.startsWith('grok-') || model === 'grok-imagine-image-edit'
    : model !== 'grok-imagine-image-edit')
  if (candidates.includes(current)) return current
  const preferred = current.startsWith('grok-')
    ? (hasReferences ? 'grok-imagine-image-edit' : 'grok-imagine-image-pro')
    : 'gpt-image-2'
  return candidates.includes(preferred) ? preferred : candidates[0] || ''
}

export function validateStudioImageModel(model: string, hasReferences: boolean) {
  if (!model) throw new Error('当前没有可用于此操作的图片模型')
  if (model.startsWith('grok-') && (hasReferences !== (model === 'grok-imagine-image-edit'))) {
    throw new Error(hasReferences ? '请为参考图选择图片编辑模型' : '图片编辑模型需要至少一张参考图')
  }
}

export interface StudioVideoOptions {
  seconds: number
  size: string
  resolution: string
}
export const DEFAULT_VIDEO_OPTIONS: StudioVideoOptions = { seconds: 6, size: '720x1280', resolution: '720p' }

export function buildVideoForm(prompt: string, options: StudioVideoOptions, files: File[]) {
  if (![6, 10, 15].includes(options.seconds)) throw new Error('视频时长仅支持 6、10、15 秒')
  if (!['720x1280', '1280x720', '1024x1024'].includes(options.size)) throw new Error('请选择支持的视频比例')
  if (!['480p', '720p'].includes(options.resolution)) throw new Error('请选择支持的视频分辨率')
  validateStudioReferenceCount('video', '', files.length)
  const form = new FormData()
  form.set('model', 'grok-imagine-video')
  form.set('prompt', prompt)
  form.set('seconds', String(options.seconds))
  form.set('size', options.size)
  form.set('resolution_name', '720p')
  files.forEach(file => form.append('input_reference', file))
  return form
}
