<template>
  <div class="studio-video-result">
    <p>{{ message.content }}</p>
    <progress v-if="message.status === 'running' || message.status === 'queued'" :value="message.videoProgress || 0" max="100" aria-label="视频生成进度" />
    <template v-if="message.status === 'done' && message.videoTaskId">
      <video v-if="url" :src="url" controls playsinline preload="metadata" class="w-full rounded-xl" />
      <p v-else-if="loading">正在加载视频…</p>
      <p v-if="error" role="alert">{{ error }}</p>
      <button v-if="!url && !loading" type="button" @click="load">加载视频</button>
      <a v-if="url" :href="url" :download="`${message.videoTaskId}.mp4`">下载视频</a>
    </template>
  </div>
</template>
<script setup lang="ts">
import { ref, watch, onBeforeUnmount, onDeactivated } from 'vue'
import { videosApi } from '@/api/videos'
import type { StudioMessage } from './types'
const props = defineProps<{ message: StudioMessage }>()
const url = ref('')
const error = ref('')
const loading = ref(false)
let request: AbortController | undefined
function cleanup() { request?.abort(); if (url.value) URL.revokeObjectURL(url.value); url.value = ''; loading.value = false }
async function load() {
  if (!props.message.videoTaskId || loading.value) return
  cleanup()
  const controller = new AbortController()
  request = controller
  loading.value = true
  error.value = ''
  try {
    const blob = await videosApi.content(props.message.videoTaskId, controller.signal)
    if (!controller.signal.aborted) url.value = URL.createObjectURL(blob)
  } catch {
    if (!controller.signal.aborted) error.value = '视频加载失败，可重试加载；不会重新生成视频。'
  } finally { if (request === controller) loading.value = false }
}
watch(
  () => [props.message.status, props.message.videoTaskId],
  ([status]) => {
    cleanup()
    if (status === 'done') void load()
  },
  { immediate: true },
)
onBeforeUnmount(cleanup)
onDeactivated(cleanup)
</script>
