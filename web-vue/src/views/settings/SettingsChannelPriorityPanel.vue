<template>
  <FormSection title="模型渠道优先级">
    <p class="text-xs leading-5 text-muted-foreground">同一模型有多个渠道时，按下方顺序选择符合参数、账号状态与额度要求的渠道。调用方只选择模型；选定渠道后失败不自动重新生成。</p>
    <p v-if="loaded && !routes.length" class="text-sm text-muted-foreground">当前没有已确认可用的多渠道模型，无需设置优先级。</p>
    <div v-for="route in routes" :key="route.model" class="space-y-2 rounded border p-3">
      <div class="text-sm font-medium">{{ route.name || route.model }}</div>
      <ol class="space-y-2">
        <li v-for="(channel, index) in route.priority" :key="channel" class="flex items-center gap-2 text-sm">
          <span class="min-w-6 text-muted-foreground">{{ index + 1 }}.</span><span class="flex-1">{{ channelLabel(channel) }}</span>
          <Button size="sm" variant="outline" :disabled="busy || index === 0" :aria-label="`提高 ${channelLabel(channel)} 优先级`" @click="move(route, index, -1)">上移</Button>
          <Button size="sm" variant="outline" :disabled="busy || index === route.priority.length - 1" :aria-label="`降低 ${channelLabel(channel)} 优先级`" @click="move(route, index, 1)">下移</Button>
        </li>
      </ol>
    </div>
    <p v-if="error" role="alert" class="text-xs text-rose-500">{{ error }}</p>
    <p v-if="message" role="status" class="text-xs text-emerald-600">{{ message }}</p>
    <div class="flex gap-2"><Button size="sm" variant="outline" :disabled="busy" @click="load">刷新渠道</Button><Button v-if="routes.length" size="sm" :disabled="busy || !loaded || !dirty" @click="save">保存优先级</Button></div>
  </FormSection>
</template>
<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { Button, FormSection } from 'nanocat-ui'
import { modelRoutingApi, type ModelRoute, type ModelRouting } from '@/api/modelRouting'
const routes = ref<ModelRoute[]>([]), busy = ref(false), loaded = ref(false), error = ref(''), message = ref(''), saved = ref('')
const snapshot = computed(() => JSON.stringify(Object.fromEntries(routes.value.map(route => [route.model, route.priority]))))
const dirty = computed(() => saved.value !== snapshot.value)
function channelLabel(channel: string) { return ({ openai: 'ChatGPT', chatgpt: 'ChatGPT', grok: 'Grok', adobe: 'Adobe' } as Record<string, string>)[channel] || channel }
function accept(data: ModelRouting) { routes.value = data.models.filter(route => route.channels.length > 1).map(route => ({ ...route, priority: [...route.priority] })); saved.value = snapshot.value; loaded.value = true }
function move(route: ModelRoute, index: number, delta: number) { const other = index + delta; if (other < 0 || other >= route.priority.length) return; const current = route.priority[index]!; route.priority[index] = route.priority[other]!; route.priority[other] = current; message.value = '' }
async function load() { busy.value = true; error.value = ''; message.value = ''; try { accept(await modelRoutingApi.get()) } catch (cause) { error.value = cause instanceof Error ? cause.message : '渠道配置加载失败' } finally { busy.value = false } }
async function save() { busy.value = true; error.value = ''; message.value = ''; try { accept(await modelRoutingApi.update(JSON.parse(snapshot.value))); message.value = '渠道优先级已保存；已有任务继续使用原渠道。' } catch (cause) { error.value = cause instanceof Error ? cause.message : '渠道配置保存失败' } finally { busy.value = false } }
onMounted(load)
</script>
