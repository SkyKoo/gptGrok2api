// Keep endpoint selection and model selection consistent, including saved selections.
export function resolveStudioImageModel(current: string, hasReferences: boolean, available: string[]) {
  const candidates = available.filter(model => hasReferences
    ? !model.startsWith('grok-') || model === 'grok-imagine-image-edit'
    : model !== 'grok-imagine-image-edit')
  if (candidates.includes(current)) return current
  const preferred = current.startsWith('grok-')
    ? (hasReferences ? 'grok-imagine-image-edit' : 'grok-imagine-image')
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
  if (files.length > 7) throw new Error('视频最多支持 7 张参考图')
  const form = new FormData()
  form.set('model', 'grok-imagine-video')
  form.set('prompt', prompt)
  form.set('seconds', String(options.seconds))
  form.set('size', options.size)
  form.set('resolution_name', options.resolution)
  files.forEach(file => form.append('input_reference', file))
  return form
}
