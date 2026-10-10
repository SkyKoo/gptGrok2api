import { computed, ref, watch } from 'vue'
import type { GalleryFile } from '@/api/gallery'
import type { SystemLogRow } from '@/api/logs'
import apiClient from '@/api/client'
import {
  buildDiagnosticDetailFields,
  buildPrimaryDetailFields,
  buildTimelineGroups,
  buildTimelineLegendItems,
  buildTimelineSegments,
  shouldAutoExpandTimeline,
  type DetailTimelineStep,
} from '@/views/logs/logDetailView'
import {
  buildLogPreviewGalleryFile,
  buildLogPreviewImages,
  type LogPreviewImage,
} from '@/views/logs/logsView'

export type DetailPreviewImage = LogPreviewImage

export function useLogDetailRuntime() {
  const selectedLog = ref<SystemLogRow | null>(null)
  const selectedDetailPreview = ref<DetailPreviewImage | null>(null)
  const timelineDetailsExpanded = ref(false)
  const brokenPreviewUrls = ref<Set<string>>(new Set())

  const selectedTimelineSegments = computed(() => buildTimelineSegments(selectedLog.value))
  const selectedTimelineLegendItems = computed(() => buildTimelineLegendItems(selectedTimelineSegments.value))
  const selectedTimelineGroups = computed(() => buildTimelineGroups(selectedLog.value))

  const selectedBottleneckStep = computed<DetailTimelineStep | null>(() => {
    const steps = selectedTimelineGroups.value.flatMap((group) => group.steps)
    return steps.reduce<DetailTimelineStep | null>((current, step) => {
      if (!current || step.valueMs > current.valueMs) return step
      return current
    }, null)
  })

  const selectedTimelineStepCount = computed(() => selectedTimelineGroups.value.reduce((total, group) => total + group.steps.length, 0))
  const selectedTimelineSegmentTotal = computed(() => selectedTimelineSegments.value.reduce((total, segment) => total + segment.valueMs, 0))
  const timelineDetailsAutoExpanded = computed(() => shouldAutoExpandTimeline(selectedLog.value, selectedBottleneckStep.value))
  const timelineDetailsVisible = computed(() => timelineDetailsExpanded.value)
  const selectedHasTimeline = computed(() => selectedTimelineSegments.value.length > 0 || selectedTimelineGroups.value.length > 0)

  const selectedPrimaryDetailFields = computed(() => buildPrimaryDetailFields(selectedLog.value))
  const selectedDiagnosticDetailFields = computed(() => buildDiagnosticDetailFields(selectedLog.value))

  const loadedInputImages = ref<DetailPreviewImage[]>([])
  const selectedDetailInputImages = computed(() => loadedInputImages.value.map((image) => ({
    ...image, broken: image.broken || isPreviewBroken(image.url),
  })))
  // Fetch only when the drawer opens; the API list never transfers image bytes.
  // Cancel stale loads and revoke temporary object URLs on switch/close/unmount.
  watch(selectedLog, (log, _previous, onCleanup) => {
    const controller = new AbortController()
    const objectUrls: string[] = []
    selectedDetailPreview.value = null
    brokenPreviewUrls.value = new Set()
    onCleanup(() => {
      controller.abort()
      objectUrls.forEach((url) => URL.revokeObjectURL(url))
    })
    const inputs = log?.inputImages || []
    loadedInputImages.value = inputs.map((input) => ({
      url: '',
      filename: input.filename || `reference-${input.index}`,
      label: `参考图 ${input.index}${input.width && input.height ? ` · ${input.width} × ${input.height}` : ''}`,
      alt: `输入参考图 ${input.index}`,
      loading: Boolean(input.url),
      broken: !input.url,
      unavailableText: '参考图未保存或不可用',
    }))
    inputs.forEach(async (input, index) => {
      if (!input.url) return
      try {
        const blob = await apiClient.get<never, Blob>(input.url, { responseType: 'blob', signal: controller.signal })
        if (controller.signal.aborted) return
        const url = URL.createObjectURL(blob)
        objectUrls.push(url)
        loadedInputImages.value[index] = { ...loadedInputImages.value[index], url, title: url, loading: false }
      } catch {
        if (!controller.signal.aborted) {
          loadedInputImages.value[index] = {
            ...loadedInputImages.value[index], loading: false, broken: true, unavailableText: '参考图已过期或无法加载',
          }
        }
      }
    })
  })

  const selectedDetailImages = computed(() => buildLogPreviewImages(selectedLog.value, isPreviewBroken))
  const selectedDetailPreviewFile = computed<GalleryFile | null>(() => buildLogPreviewGalleryFile(selectedDetailPreview.value))

  function isPreviewBroken(url: string): boolean {
    return brokenPreviewUrls.value.has(url)
  }

  function markPreviewBroken(event: Event, url: string) {
    const img = event.target as HTMLImageElement
    img.style.opacity = '0'
    brokenPreviewUrls.value = new Set([...brokenPreviewUrls.value, url])
  }

  function openDetail(item: SystemLogRow) {
    selectedLog.value = item
  }

  function closeDetail() {
    selectedLog.value = null
    selectedDetailPreview.value = null
  }

  function openDetailImagePreview(image: DetailPreviewImage) {
    selectedDetailPreview.value = image
  }

  function closeDetailImagePreview() {
    selectedDetailPreview.value = null
  }

  function toggleTimelineDetails() {
    timelineDetailsExpanded.value = !timelineDetailsExpanded.value
  }

  watch(
    () => selectedLog.value?.id || '',
    () => {
      timelineDetailsExpanded.value = timelineDetailsAutoExpanded.value
    },
  )

  return {
    selectedLog,
    selectedDetailPreview,
    selectedDetailPreviewFile,
    selectedDetailImages,
    selectedDetailInputImages,
    selectedPrimaryDetailFields,
    selectedDiagnosticDetailFields,
    selectedTimelineSegments,
    selectedTimelineLegendItems,
    selectedTimelineGroups,
    selectedBottleneckStep,
    selectedTimelineStepCount,
    selectedTimelineSegmentTotal,
    selectedHasTimeline,
    timelineDetailsVisible,
    isPreviewBroken,
    markPreviewBroken,
    openDetail,
    closeDetail,
    openDetailImagePreview,
    closeDetailImagePreview,
    toggleTimelineDetails,
  }
}
