import { mediaCapability, mediaReferenceLimit, validateMediaCombination } from './mediaCapabilities.ts'
export const GROK_IMAGE_REFERENCE_LIMIT = 5
export const GROK_VIDEO_REFERENCE_LIMIT = 7

export function studioReferenceLimit(mode: string, imageModel: string) {
  const confirmed = mediaReferenceLimit(imageModel)
  if ((mode === 'video' || mode === 'image') && confirmed !== undefined) return confirmed
  if (mode === 'video') return GROK_VIDEO_REFERENCE_LIMIT
  if (mode === 'image' && imageModel.startsWith('grok-')) return GROK_IMAGE_REFERENCE_LIMIT
  return 8
}

export function studioReferenceCountError(mode: string, imageModel: string, count: number) {
  const limit = studioReferenceLimit(mode, imageModel)
  if (count <= limit) return ''
  const label = mode === 'video' ? '当前视频模型' : mode === 'image' && imageModel.startsWith('grok-') ? 'Grok 生图' : '当前模式'
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
  model?: string
  audio?: boolean
  seconds: number
  size: string
  resolution: string
  preset?: string
}
export const VIDEO_SIZE_OPTIONS = [
  { label: '9:16 竖屏', value: '720x1280' },
  { label: '16:9 横屏', value: '1280x720' },
  { label: '1:1 方形', value: '1024x1024' },
  { label: '2:3', value: '1024x1792' },
  { label: '3:2', value: '1792x1024' },
]
export const VIDEO_DURATION_OPTIONS = [1, 2, 3, 4, 5, 6].map(seconds => ({ label: `${seconds} 秒`, value: String(seconds) }))
export const VIDEO_PRESET_OPTIONS = [
  { label: '自定义', value: 'custom' }, { label: '普通', value: 'normal' },
  { label: '趣味', value: 'fun' }, { label: '大胆', value: 'spicy' },
]
export const DEFAULT_VIDEO_OPTIONS: StudioVideoOptions = { model: 'grok-imagine-video', audio: false, seconds: 6, size: '720x1280', resolution: '720p', preset: 'custom' }

export function buildVideoForm(prompt: string, options: StudioVideoOptions, files: File[]) {
  const model = options.model || 'grok-imagine-video'
  const capability = mediaCapability(model)
  if (!capability && model !== 'grok-imagine-video') throw new Error('视频模型能力尚未加载，请刷新模型目录')
  validateMediaCombination(model, { size: options.size, seconds: options.seconds, resolution: options.resolution, audio: options.audio, references: files.length })
  if (!capability && !VIDEO_DURATION_OPTIONS.some(option => Number(option.value) === options.seconds)) throw new Error('请选择 1～6 秒的视频时长')
  if (!capability && !VIDEO_SIZE_OPTIONS.some(option => option.value === options.size)) throw new Error('请选择支持的视频比例')
  if (!capability && !['480p', '720p'].includes(options.resolution)) throw new Error('请选择支持的视频分辨率')
  const preset = options.preset || 'custom'
  if (model.startsWith('grok-') && !VIDEO_PRESET_OPTIONS.some(option => option.value === preset)) throw new Error('请选择支持的视频风格')
  validateStudioReferenceCount('video', model, files.length)
  const form = new FormData()
  form.set('model', model)
  form.set('prompt', prompt)
  form.set('seconds', String(options.seconds))
  form.set('size', options.size)
  form.set('resolution_name', options.resolution)
  if (model.startsWith('grok-')) form.set('preset', preset)
  if (!model.startsWith('grok-') && options.audio !== undefined) form.set('generate_audio', String(options.audio))
  files.forEach(file => form.append('input_reference', file))
  return form
}
