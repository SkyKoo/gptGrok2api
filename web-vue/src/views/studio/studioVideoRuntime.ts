import { ref, type Ref } from 'vue'
import { videosApi, type VideoTask } from '@/api/videos'
import type { StudioConversation, StudioMessage } from '@/components/studio/types'

export function applyVideoTask(message: StudioMessage, task: VideoTask) {
  message.videoTaskId = task.id
  message.videoProgress = Math.max(0, Math.min(100, Number(task.progress) || 0))
  message.status = task.status === 'completed' ? 'done' : task.status === 'failed' ? 'error' : 'running'
  message.error = task.status === 'failed' ? task.error?.message || '视频生成失败' : undefined
  message.content = task.status === 'completed' ? '视频已生成' : message.error || `视频生成中 ${message.videoProgress}%`
}

export function useStudioVideoRuntime(input: {
  conversations: Ref<StudioConversation[]>
  touch: (conversation: StudioConversation) => void
  notice: (id: string, state: 'done' | 'error' | 'running') => void
}) {
  let timer: ReturnType<typeof setTimeout> | undefined
  let active = true
  let polling = false
  const pollError = ref('')
  async function poll() {
    if (!active || polling) return
    polling = true
    try {
      for (const conversation of input.conversations.value) {
        for (const message of conversation.messages) {
          if (!active) return
          if (!message.videoTaskId || !['queued', 'running'].includes(message.status || '')) continue
          try {
            const task = await videosApi.get(message.videoTaskId)
            // The user may have removed a conversation while the request was in flight.
            if (!input.conversations.value.includes(conversation) || !conversation.messages.includes(message)) continue
            applyVideoTask(message, task)
            pollError.value = ''
            input.touch(conversation)
            if (message.status === 'done' || message.status === 'error') input.notice(conversation.id, message.status)
          } catch (error) {
            if ((error as { status?: number }).status === 404) {
              message.status = 'error'
              message.error = '视频任务不存在或服务已重启，请检查调用日志；不会自动重新生成。'
              message.content = message.error
              input.touch(conversation)
              input.notice(conversation.id, 'error')
            } else {
              pollError.value = '视频进度查询暂时失败，稍后继续查询，不会重复提交。'
            }
          }
        }
      }
    } finally {
      polling = false
      if (active) timer = setTimeout(poll, 3000)
    }
  }
  function start() { active = true; clearTimeout(timer); void poll() }
  function stop() { active = false; clearTimeout(timer) }
  function apply(message: StudioMessage, task: VideoTask) { applyVideoTask(message, task) }
  return { start, stop, pollError, apply }
}
