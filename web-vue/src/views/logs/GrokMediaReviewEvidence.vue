<template>
  <section v-if="reviews.length" class="space-y-3">
    <h3 class="text-sm font-semibold">Grok 内容核对与重试证据</h3>
    <article v-for="(review, index) in reviews" :key="index" class="space-y-3 rounded-lg border p-3 text-xs">
      <p class="font-medium">
        第 {{ review.attempt }} 次生成 · {{ review.kind === 'video' ? '视频' : `图片 ${review.output}` }} ·
        {{ verdictLabel(review.verdict) }}{{ review.cache_hit ? '（复用哈希核对记录，未调用模型）' : '' }}
      </p>
      <p>原因：{{ review.reason }} <span v-if="review.model">· 核对模型：{{ review.model }}</span></p>
      <p v-if="review.expected">请求主要内容：{{ review.expected }}</p>
      <p v-if="review.observed">实际画面：{{ review.observed }}</p>
      <p v-if="review.basis">判断依据：{{ review.basis }}</p>
      <p v-if="review.persistence_error" class="text-destructive">哈希记录保存失败：{{ review.persistence_error }}</p>
      <details class="break-all text-muted-foreground">
        <summary class="cursor-pointer">哈希与来源记录</summary>
        <p>内容 SHA-256：{{ review.media_sha256 }}</p>
        <p>文件 SHA-256：{{ review.file_sha256 }}</p>
        <p>请求 SHA-256：{{ review.request_sha256 }}</p>
        <p>来源任务：{{ review.source_call_id }} · {{ review.at }}</p>
      </details>
      <DetailImagePreview :title="review.kind === 'video' ? '实际送审视频帧（按时间顺序）' : '实际送审图片'"
        :images="previews[index]?.images || []" @preview-click="$emit('preview-click', $event)" @image-error="markBroken" />
      <DetailImagePreview title="用于核对的参考图" :images="previews[index]?.references || []"
        @preview-click="$emit('preview-click', $event)" @image-error="markBroken" />
      <details v-for="(check, checkIndex) in review.checks || []" :key="checkIndex">
        <summary class="cursor-pointer">第 {{ checkIndex + 1 }} 次核对回复 · {{ verdictLabel(check.verdict) }} · {{ check.parse_error || check.reason }}{{ check.truncated ? '（超长截断）' : '' }}</summary>
        <p v-if="check.parse_notes?.length" class="mt-2 text-muted-foreground">格式兼容：{{ check.parse_notes.map(formatNote).join('；') }}</p>
        <p v-if="check.error" class="mt-2 text-destructive">{{ check.error }}</p>
        <pre class="mt-2 max-h-60 overflow-auto whitespace-pre-wrap break-all rounded bg-muted p-2">{{ check.response || '未取得可用回复' }}</pre>
      </details>
      <p v-if="review.cache_hit" class="text-muted-foreground">画面和依据来自原核对记录；预览按图片保留策略清理，哈希记录继续保留。</p>
    </article>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import apiClient from '@/api/client'
import { normalizeLogInputImages } from '@/api/logInputImages'
import DetailImagePreview, { type DetailImagePreviewItem } from '@/components/ai/DetailImagePreview.vue'
import type { DetailPreviewImage } from '@/views/logs/logDetailRuntime'
const props = defineProps<{ entries?: unknown }>()
defineEmits<{ (e: 'preview-click', image: DetailPreviewImage): void }>()
const reviews = computed(() => Array.isArray(props.entries) ? props.entries.filter(item => item && typeof item === 'object').slice(0, 20) : [])
const previews = ref<{ images: DetailImagePreviewItem[]; references: DetailImagePreviewItem[] }[]>([])
function verdictLabel(verdict: string) {
  return verdict === 'matched' ? '主要内容相关' : verdict === 'mismatch' ? '确认明显无关' : '核对故障或证据不足（非内容不符）'
}
function formatNote(note: string) {
  const labels: Record<string, string> = {
    json_envelope_removed: '已提取说明文字或代码围栏中的完整 JSON',
    trailing_commas_removed: '已去除 JSON 尾随逗号',
    normalized_field_names: '已统一字段名写法',
    normalized_verdict: '已统一结论大小写和空格',
    normalized_reason: '已统一原因写法',
    reason_normalized_from_verdict: '以明确结论规范原因字段',
    additional_fields_ignored: '已忽略无关的额外字段',
    identical_duplicate_field_ignored: '已合并内容相同的重复字段',
    duplicate_count_ignored: '重复计数不影响内容结论',
    frames_reviewed_uses_actual_count: '以 CFM 实际送审数量替代模型自报的生成画面数量',
    references_reviewed_uses_actual_count: '以 CFM 实际送审数量替代模型自报的参考图数量',
    string_count_normalized: '已兼容字符串形式的计数',
    single_review_wrapper_removed: '已提取包装对象中的单一审核结果',
    repeated_identical_review: '已合并重复且一致的审核结果',
    non_text_expected_ignored: '主要意图字段不是文本，未使用',
    non_text_observed_ignored: '画面描述字段不是文本，未使用',
    non_text_basis_ignored: '依据字段不是文本，未使用',
  }
  return labels[note] || note
}
function markBroken(event: Event) {
  const target = event.target as HTMLImageElement
  target.style.opacity = '0'
}
watch(reviews, (entries, _previous, onCleanup) => {
  const controller = new AbortController()
  const objectUrls: string[] = []
  onCleanup(() => { controller.abort(); objectUrls.forEach(url => URL.revokeObjectURL(url)) })
  previews.value = entries.map(() => ({ images: [], references: [] }))
  entries.forEach((entry, index) => {
    for (const kind of ['images', 'references'] as const) {
      const images = normalizeLogInputImages({ input_images: entry[kind] })
      previews.value[index][kind] = images.map((image, i) => ({ url: '', label: `${kind === 'images' ? '生成画面' : '参考图'} ${i + 1}`, loading: Boolean(image.url), broken: !image.url, unavailableText: '证据预览未保存或已过期' }))
      images.forEach(async (image, i) => {
        if (!image.url) return
        try {
          const blob = await apiClient.get<never, Blob>(image.url, { responseType: 'blob', signal: controller.signal })
          if (controller.signal.aborted) return
          const url = URL.createObjectURL(blob)
          objectUrls.push(url)
          previews.value[index][kind][i] = { ...previews.value[index][kind][i], url, loading: false }
        } catch {
          if (!controller.signal.aborted) previews.value[index][kind][i] = { ...previews.value[index][kind][i], loading: false, broken: true }
        }
      })
    }
  })
}, { immediate: true })
</script>
