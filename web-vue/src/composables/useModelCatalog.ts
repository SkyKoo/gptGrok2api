import { setMediaCapabilities } from '@/lib/mediaCapabilities'
import { computed, ref } from 'vue'
import { modelsApi } from '@/api/models'
import type { ModelCatalogResponse, ModelListResponse } from '@/api/models'
import type { Settings } from '@/types/api'
import {
  isImageModelId,
  resolveImageModels,
} from '@/config/modelCatalog'

type SettingsResolver = () => Settings | null | undefined

const sharedCatalog = ref<ModelCatalogResponse | null>(null)
const loadError = ref<Error | null>(null)
const isLoading = ref(false)

let hasLoaded = false
let inflight: Promise<ModelCatalogResponse | null> | null = null

function normalizeList(raw: unknown): string[] {
  if (!Array.isArray(raw)) return []
  const result: string[] = []
  for (const item of raw) {
    const value = String(item || '').trim()
    if (!value || result.includes(value)) continue
    result.push(value)
  }
  return result
}

function normalizeCatalog(payload: ModelCatalogResponse | null | undefined): ModelCatalogResponse | null {
  if (!payload) return null
  const chatModels = normalizeList(payload.chat_models)
  const imageModels = normalizeList(payload.image_models)
  const imageEditModels = normalizeList(payload.image_edit_models)
  const videoModels = normalizeList(payload.video_models)
  return {
    ...payload,
    chat_models: chatModels,
    image_models: imageModels,
    image_edit_models: imageEditModels,
    video_models: videoModels,
    all_models: normalizeList(payload.all_models).length
      ? normalizeList(payload.all_models)
      : normalizeList([...chatModels, ...imageModels, ...imageEditModels, ...videoModels]),
  }
}

function catalogFromOpenAIModels(response: ModelListResponse): ModelCatalogResponse | null {
  const rows = Array.isArray(response.data) ? response.data : []
  const ids = normalizeList(rows.map(item => item?.id))
  if (ids.length === 0) return null
  const capabilities = Object.fromEntries(rows.filter(item => item.media_capabilities).map(item => [item.id, item.media_capabilities!]))
  const isImage = (id: string) => capabilities[id]?.kind === 'image' || isImageModelId(id)
  const isVideo = (id: string) => capabilities[id]?.kind === 'video' || id.toLowerCase().includes('video') || id.startsWith('veo-')
  return {
    object: 'model_catalog',
    chat_models: ids.filter(model => !isImage(model) && !isVideo(model)),
    image_models: ids.filter(model => isImage(model) && model !== 'grok-imagine-image-edit'),
    image_edit_models: ids.filter(model => isImage(model) && (capabilities[model]?.max_reference_images ?? 8) > 0 && (!model.startsWith('grok-') || model.includes('edit'))),
    video_models: ids.filter(isVideo),
    media_capabilities: capabilities,
    all_models: ids,
    models: response.data,
    source: { chat: 'openai_models_endpoint', image: 'openai_models_endpoint' },
    openai_models_endpoint: '/v1/models',
  }
}

export function useModelCatalog(resolveSettings: SettingsResolver) {
  const chatModels = computed(() => {
    const fromCatalog = normalizeList(sharedCatalog.value?.chat_models)
    return fromCatalog
  })

  const modelLabels = computed(() => Object.fromEntries((sharedCatalog.value?.models || []).filter(item => item.name).map(item => [item.id, item.name!])))
  const chatModelLabels = computed(() => Object.fromEntries(Object.entries(modelLabels.value).filter(([id]) => id.startsWith('grok-web-'))))
  const modelAliases = computed(() => sharedCatalog.value?.model_aliases || {})

  const imageModels = computed(() => {
    const fromCatalog = normalizeList(sharedCatalog.value?.image_models)
    return sharedCatalog.value ? fromCatalog : resolveImageModels(resolveSettings())
  })

  const imageEditModels = computed(() => {
    const fromCatalog = normalizeList(sharedCatalog.value?.image_edit_models)
    return sharedCatalog.value?.image_edit_models !== undefined
      ? fromCatalog
      : imageModels.value.filter(model => !model.startsWith('grok-') || model.includes('edit'))
  })

  const videoModels = computed(() => normalizeList(sharedCatalog.value?.video_models))

  async function loadModelCatalog(force = false) {
    if (!force && hasLoaded) return sharedCatalog.value
    if (inflight) return inflight

    isLoading.value = true
    inflight = (async () => {
      hasLoaded = true
      try {
        const catalog = normalizeCatalog(await modelsApi.catalog())
        setMediaCapabilities(catalog?.media_capabilities)
        sharedCatalog.value = catalog
        loadError.value = null
        return catalog
      } catch (catalogError) {
        try {
          const fallback = normalizeCatalog(catalogFromOpenAIModels(await modelsApi.list()))
          setMediaCapabilities(fallback?.media_capabilities)
          sharedCatalog.value = fallback
          loadError.value = null
          return fallback
        } catch (listError) {
          setMediaCapabilities()
          sharedCatalog.value = null
          loadError.value = listError instanceof Error ? listError : new Error('Failed to load model catalog')
          console.error('Failed to load model catalog:', catalogError, listError)
          return null
        }
      } finally {
        isLoading.value = false
        inflight = null
      }
    })()

    return inflight
  }

  return {
    catalog: sharedCatalog,
    chatModels,
    chatModelLabels,
    modelLabels,
    modelAliases,
    imageModels,
    imageEditModels,
    videoModels,
    isLoading,
    loadError,
    loadModelCatalog,
  }
}
