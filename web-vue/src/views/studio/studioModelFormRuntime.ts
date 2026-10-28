import { resolveStudioImageModel } from '@/lib/studioMedia'
import { computed, reactive, ref, watch } from 'vue'
import {
  DEFAULT_IMAGE_MODEL,
  DEFAULT_IMAGE_QUALITY,
  DEFAULT_IMAGE_SIZE,
  isImageSizeSupportedByModel,
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
}

export function useStudioModelFormRuntime(input: StudioModelFormRuntimeInput) {
  const { chatModels, imageModels, imageEditModels, loadModelCatalog, loadError, isLoading } = useModelCatalog(() => input.settingsStore.settings)
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
      : [DEFAULT_IMAGE_MODEL, 'grok-imagine-image']
    return uniqueStrings([...catalogModels, ...fallbackModels])
      .filter(model => hasReferences
        ? !model.startsWith('grok-') || model === 'grok-imagine-image-edit'
        : model !== 'grok-imagine-image-edit')
  })
  watch([imageModelOptions, () => input.hasReferences(), () => imageForm.model], () => {
    imageForm.model = resolveStudioImageModel(imageForm.model, input.hasReferences(), imageModelOptions.value)
    if (imageForm.model === 'grok-imagine-image-edit') imageForm.n = Math.min(2, imageForm.n)
  }, { immediate: true, flush: 'sync' })

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
    chatReasoningEffort,
    imageForm,
    imageModelOptions,
    loadModelCatalog,
  }
}

function uniqueStrings(values: string[]) {
  return values.map((value) => String(value || '').trim()).filter((value, index, arr) => value && arr.indexOf(value) === index)
}
