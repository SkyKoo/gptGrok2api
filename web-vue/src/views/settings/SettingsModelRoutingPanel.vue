<template>
  <FormSection title="ChatGPT 模型路由">
    <p class="text-xs leading-5 text-muted-foreground">聊天按调用方指定的模型执行，auto 由上游选择。生图对外使用 gpt-image-2，下方选择触发图片工具的会话模型；目录可见不代表所有编辑能力都已验证。</p>
    <FormField label="生图会话模型">
      <select v-model="selected" class="ui-input w-full" :disabled="busy || !loaded">
        <option v-for="id in choices" :key="id" :value="id">{{ id }}</option>
      </select>
    </FormField>
    <p v-if="message" role="status" class="text-xs" :class="failed ? 'text-rose-500' : 'text-emerald-600'">{{ message }}</p>
    <div class="flex gap-2">
      <Button size="sm" variant="outline" :disabled="busy" @click="load">刷新目录</Button>
      <Button size="sm" variant="primary" :disabled="busy || !loaded || !selected" @click="save">{{ busy ? '处理中…' : '保存模型路由' }}</Button>
    </div>
    <p class="text-xs text-muted-foreground">此处单独保存。变更对后续开始执行的请求生效；失败重试只切换账号，不自动改成另一个模型。</p>
  </FormSection>
</template>
<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { Button, FormField, FormSection } from 'nanocat-ui'
import { apiClient } from '@/api/client'
type Routing = { image_conversation_model: string; image_model_choices: string[] }
const selected = ref(''), choices = ref<string[]>([]), busy = ref(false), loaded = ref(false), message = ref(''), failed = ref(false)
function accept(data: Routing) { selected.value = data.image_conversation_model; choices.value = [...new Set([data.image_conversation_model, ...data.image_model_choices])]; loaded.value = true }
async function load() {
  busy.value = true; message.value = ''; failed.value = false
  try { accept(await apiClient.get<unknown, Routing>('/api/openai/routing')) }
  catch (e) { message.value = e instanceof Error ? e.message : '模型路由加载失败'; failed.value = true }
  finally { busy.value = false }
}
async function save() {
  busy.value = true; message.value = ''; failed.value = false
  try { accept(await apiClient.post<unknown, Routing>('/api/openai/routing', { image_conversation_model: selected.value })); message.value = '模型路由已保存' }
  catch (e) { message.value = e instanceof Error ? e.message : '模型路由保存失败'; failed.value = true }
  finally { busy.value = false }
}
onMounted(load)
</script>
