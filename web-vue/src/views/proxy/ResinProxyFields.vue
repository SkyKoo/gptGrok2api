<template>
  <div class="mt-3 grid grid-cols-1 gap-3 md:grid-cols-3">
    <label class="text-xs">
      <span class="ui-field-label">Resin 地址</span>
      <Input v-model="fields.address" block placeholder="http://resin:2260" @update:model-value="update" />
    </label>
    <label class="text-xs">
      <span class="ui-field-label">Platform</span>
      <Input v-model="fields.platform" block placeholder="CFM-Grok" @update:model-value="update" />
    </label>
    <label class="text-xs">
      <span class="ui-field-label">代理 Token</span>
      <Input v-model="fields.token" type="password" block autocomplete="new-password" @update:model-value="update" />
    </label>
    <p class="text-xs text-muted-foreground md:col-span-3">
      已启用任务粘性出口：Account 自动生成，整个注册任务及重试复用同一身份，成功后保存到账号。节点失效或租约过期仍可能更换出口；请在 Resin 设置足够的租约时长。
    </p>
  </div>
</template>

<script setup lang="ts">
import { reactive, watch } from 'vue'
import { Input } from 'nanocat-ui'
import { buildResinProxyURL, readResinProxyURL } from './resinProxyForm'
const props = defineProps<{ modelValue: string }>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()
const fields = reactive(readResinProxyURL(props.modelValue))
function update() { emit('update:modelValue', buildResinProxyURL(fields)) }
watch(() => props.modelValue, value => {
  if (value !== buildResinProxyURL(fields)) Object.assign(fields, readResinProxyURL(value))
})
</script>
