<template>
  <div class="mt-3 grid grid-cols-1 gap-3 md:grid-cols-3">
    <label class="text-xs">
      <span class="ui-field-label">Resin 地址</span>
      <Input v-model="fields.address" :disabled="disabled" block placeholder="http://resin:2260" @update:model-value="update" />
    </label>
    <label class="text-xs">
      <span class="ui-field-label">Platform</span>
      <Input v-model="fields.platform" :disabled="disabled" block placeholder="Grok-Register" @update:model-value="update" />
    </label>
    <label class="text-xs">
      <span class="ui-field-label">代理 Token</span>
      <Input v-model="fields.token" type="password" :disabled="disabled" block autocomplete="new-password" @update:model-value="update" />
    </label>
    <p class="text-xs text-muted-foreground md:col-span-3">
      已启用任务身份绑定：Account 自动生成，单次注册尝试复用同一身份，补偿子任务生成新身份，成功后保存到账号。新的身份不保证分到不同节点。Resin 在节点失效时仍可能切换出口；延长租约不能阻止故障切换。
    </p>
  </div>
</template>

<script setup lang="ts">
import { reactive, watch } from 'vue'
import { Input } from 'nanocat-ui'
import { buildResinProxyURL, readResinProxyURL } from './resinProxyForm'
const props = defineProps<{ modelValue: string; disabled?: boolean }>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()
const fields = reactive(readResinProxyURL(props.modelValue))
function update() { emit('update:modelValue', buildResinProxyURL(fields)) }
watch(() => props.modelValue, value => {
  if (value !== buildResinProxyURL(fields)) Object.assign(fields, readResinProxyURL(value))
})
</script>
