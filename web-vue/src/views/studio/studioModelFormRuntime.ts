import { matchingMediaVariants, mediaCapability } from '@/lib/mediaCapabilities'
import { resolveStudioImageModel } from '@/lib/studioMedia'
import { computed, reactive, ref, watch } from 'vue'
import {
  DEFAULT_IMAGE_MODEL,
  DEFAULT_IMAGE_QUALITY,
  DEFAULT_IMAGE_SIZE,
  isImageSizeSupportedByModel,
  resolveImageSizePresets,
} from '@/api/imageTasks'
import { useModelCatalog } from '@/composables/useModelCatalog'
import {
  getStringPreference,
  preferenceKeys,
  setStringPreference,
} from '@/lib/preferences'
import type { StudioImageForm } from '@/components/studio/types'
import type { useSettingsStore } from '@/stores/settings'

export type StudioModelFormRuntimeInput = {
  settingsStore: ReturnType<typeof useSettingsStore>
  hasReferences: () => boolean
  referenceCount?: () => number
}

export function useStudioModelFormRuntime(input: StudioModelFormRuntimeInput) {
  const referenceCount = () => input.referenceCount?.() ?? (input.hasReferences() ? 1 : 0)
  const { chatModels, chatModelLabels, modelLabels, modelAliases, imageModels, imageEditModels, videoModels, catalog, loadModelCatalog, loadError, isLoading } = useModelCatalog(() => input.settingsStore.settings)
  const chatModel = ref(getStringPreference(preferenceKeys.studioChatModel, 'auto') || 'auto')
  const chatReasoningEffort = ref(getStringPreference(preferenceKeys.studioChatReasoningEffort, ''))
  const imageForm = reactive<StudioImageForm>({
    model: getStringPreference(preferenceKeys.studioImageModel, DEFAULT_IMAGE_MODEL) || DEFAULT_IMAGE_MODEL,
    size: DEFAULT_IMAGE_SIZE,
    quality: DEFAULT_IMAGE_QUALITY,
    n: 1,
  })

  const chatModelOptions = computed(() => uniqueStrings(chatModels.value))
  const imageModelOptions = computed(() => {
    const hasReferences = input.hasReferences()
    const catalogModels = hasReferences ? imageEditModels.value : imageModels.value
    const fallbackModels = hasReferences
      ? ['gpt-image-2', 'grok-imagine-image-edit']
      : [DEFAULT_IMAGE_MODEL, 'grok-imagine-image-pro']
    return uniqueStrings(catalog.value ? catalogModels : fallbackModels)
      .filter(model => !['grok-imagine-image', 'grok-imagine-image-lite'].includes(model))
      .filter(model => hasReferences
        ? (mediaCapability(model)?.max_reference_images ?? 8) > 0 && (!model.startsWith('grok-') || model === 'grok-imagine-image-edit')
        : model !== 'grok-imagine-image-edit')
  })
  watch([imageModelOptions, referenceCount, () => imageForm.model], () => {
    imageForm.model = resolveStudioImageModel(imageForm.model, input.hasReferences(), imageModelOptions.value)
    const capability = mediaCapability(imageForm.model)
    const variants = capability ? matchingMediaVariants(capability, { references: referenceCount() }) : []
    const qualities = [...new Set(variants.flatMap(variant => variant.qualities || []))]
    if (qualities.length && !qualities.includes(imageForm.quality)) imageForm.quality = qualities[0]!
    const maxCount = Math.max(0, ...variants.map(item => item.max_count || 0)) || capability?.max_count || (imageForm.model === 'grok-imagine-image-edit' ? 2 : 10)
    imageForm.n = Math.min(input.hasReferences() ? Math.min(maxCount, 2) : maxCount, imageForm.n)
    const sizes = resolveImageSizePresets(imageForm.model, referenceCount())
    if (sizes.length && !sizes.some(item => item.value === imageForm.size)) imageForm.size = sizes[0]!.value
  }, { immediate: true, flush: 'sync' })

  watch(modelAliases, (aliases) => {
    const canonical = aliases[chatModel.value]
    if (canonical) chatModel.value = canonical
  }, { immediate: true })

  watch(chatModel, (model) => setStringPreference(preferenceKeys.studioChatModel, model || 'auto'))
  watch(chatReasoningEffort, (effort) => setStringPreference(preferenceKeys.studioChatReasoningEffort, effort || ''))
  watch(() => imageForm.model, (model) => {
    setStringPreference(preferenceKeys.studioImageModel, model || DEFAULT_IMAGE_MODEL)
    if (!isImageSizeSupportedByModel(imageForm.size, model)) imageForm.size = DEFAULT_IMAGE_SIZE
  })

  return {
    modelCatalogError: loadError,
    modelCatalogLoading: isLoading,
    chatModel,
    chatModelOptions,
    chatModelLabels,
    modelLabels,
    chatReasoningEffort,
    imageForm,
    catalog,
    videoModelOptions: videoModels,
    imageModelOptions,
    loadModelCatalog,
  }
}

function uniqueStrings(values: string[]) {
  return values.map((value) => String(value || '').trim()).filter((value, index, arr) => value && arr.indexOf(value) === index)
}
