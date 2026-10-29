<template>
  <PagePanel class="space-y-4">
    <PanelHeader title="账号固定出口" />
    <p class="text-xs text-muted-foreground">注册任务会保存代理绑定，后续操作继续使用该节点。手动切换需填写原因；正在执行的任务仍使用启动时的出口。</p>
    <div class="grid gap-3 md:grid-cols-3">
      <label class="text-xs"><span class="ui-field-label">平台</span>
        <select v-model="platform" class="w-full rounded border border-border bg-background p-2" :disabled="busy" @change="clearSelection"><option value="openai">GPT</option><option value="grok">Grok</option></select>
      </label>
      <label class="text-xs"><span class="ui-field-label">账号 ID（账号列表中的 ID）</span><Input v-model="accountId" :disabled="busy" block @update:model-value="clearSelection" /></label>
      <Button class="self-end" variant="outline" :disabled="busy || !accountId.trim()" @click="load">查询绑定</Button>
    </div>
    <template v-if="current">
      <p class="text-sm">当前出口：{{ current.label }} · {{ current.available ? '配置可用' : '配置不可用' }}</p>
      <div class="grid gap-3 md:grid-cols-3">
        <label class="text-xs"><span class="ui-field-label">新出口</span>
          <select v-model="reference" :disabled="busy" class="w-full rounded border border-border bg-background p-2"><option value="">请选择节点</option><option value="direct">直连</option><option value="custom">单个代理 URL</option><optgroup v-for="group in groups.filter(g => g.enabled)" :key="group.id" :label="group.name"><option v-for="node in group.nodes.filter(n => n.enabled)" :key="node.id" :value="nodeRef(group.id, node.id)">{{ node.name || node.id }}</option></optgroup></select>
          <Input v-if="reference === 'custom'" v-model="customURL" :disabled="busy" block placeholder="http://host:port 或 socks5://host:port" />
        </label>
        <label class="text-xs"><span class="ui-field-label">切换原因</span><Input v-model="reason" :disabled="busy" block placeholder="例如：原节点已停用" /></label>
        <Button class="self-end" variant="primary" :disabled="busy || !reference || (reference === 'custom' && !customURL.trim()) || !reason.trim()" @click="save">保存绑定并记录原因</Button>
      </div>
      <ul v-if="current.history?.length" class="space-y-1 text-xs text-muted-foreground"><li v-for="(entry, index) in current.history" :key="index">{{ entry.at }} · {{ entry.from }} → {{ entry.to }} · {{ entry.reason }}</li></ul>
    </template>
    <p v-if="message" role="status" class="text-sm">{{ message }}</p>
  </PagePanel>
</template>
<script setup lang="ts">
import { ref } from 'vue'
import { Button, Input } from 'nanocat-ui'
import PagePanel from '@/components/ai/PagePanel.vue'
import PanelHeader from '@/components/ai/PanelHeader.vue'
import { proxyApi, type AccountProxyBinding, type ProxyGroup } from '@/api/proxy'
defineProps<{ groups: ProxyGroup[] }>()
const platform = ref<'openai' | 'grok'>('openai')
const accountId = ref('')
const reference = ref('')
const reason = ref('')
const customURL = ref('')
const busy = ref(false)
const message = ref('')
const current = ref<AccountProxyBinding | null>(null)
const nodeRef = (group: string, node: string) => `node:${encodeURIComponent(group)}/${encodeURIComponent(node)}`
function clearSelection() { current.value = null; reference.value = ''; customURL.value = ''; reason.value = ''; message.value = '' }
async function load() {
  busy.value = true; message.value = ''; current.value = null
  try { current.value = await proxyApi.getBinding(platform.value, accountId.value.trim()) }
  catch { message.value = '查询失败，请确认平台、账号 ID 和管理员登录状态。' }
  finally { busy.value = false }
}
async function save() {
  if (!current.value) return
  busy.value = true; message.value = ''
  try {
    current.value = await proxyApi.saveBinding({ platform: platform.value, account_id: accountId.value.trim(), reference: reference.value === 'custom' ? customURL.value.trim() : reference.value, reason: reason.value.trim() })
    message.value = '绑定已保存，切换原因已记录。'; reason.value = ''; reference.value = ''; customURL.value = ''
  } catch { message.value = '保存失败：节点可能不可用或繁忙；请重新查询当前绑定后再处理。' }
  finally { busy.value = false }
}
</script>
