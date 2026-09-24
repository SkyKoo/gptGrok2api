<template>
  <FormSection title="ChatGPT Free 注册" density="roomy">
    <div class="grid gap-4">
      <p class="text-sm text-muted-foreground">每次注册 1 个账号，使用 iCloud HME 收取验证码。账号入库后校验，通过后加入账号池。</p>
      <label class="grid gap-1 text-sm">注册姓名
        <Input v-model="config.openai_free.name" :disabled="config.enabled" block maxlength="100" />
      </label>
      <label class="grid gap-1 text-sm">出生日期
        <Input v-model="config.openai_free.birthdate" :disabled="config.enabled" type="date" block />
      </label>
      <label class="grid gap-1 text-sm">任务总超时（秒）
        <Input v-model.number="config.openai_free.timeout_seconds" :disabled="config.enabled" type="number" min="60" max="1800" block />
      </label>
      <label class="grid gap-1 text-sm">等待验证码（秒）
        <Input v-model.number="config.mail.wait_timeout" :disabled="config.enabled" type="number" min="1" max="900" block />
      </label>
      <label class="grid gap-1 text-sm">注册代理（留空沿用全局，direct 为直连）
        <Input :model-value="config.proxy" :disabled="config.enabled" block placeholder="direct 或 http://…"
          @update:model-value="emit('proxy', String($event || ''))" />
      </label>
      <p class="text-xs text-muted-foreground">HME 服务使用独立直连连接。注册失败时保留别名，以便核查；不会自动删除邮箱或重新注册。</p>
      <Button variant="ghost" :disabled="config.enabled" @click="emit('target', 'grok')">切换 Grok 注册配置</Button>
    </div>
  </FormSection>
</template>
<script setup lang="ts">
import { Button, Input } from 'nanocat-ui'
import FormSection from '@/components/ai/FormSection.vue'
import { watch } from 'vue'
import type { LegacyRegisterConfig } from '@/api/register'
const props = defineProps<{ config: LegacyRegisterConfig }>()
const emit = defineEmits<{ (e: 'proxy', value: string): void; (e: 'target', value: string): void }>()
watch(() => props.config, (config) => {
  if (config.enabled) return
  config.mode = 'total'; config.total = 1; config.threads = 1
  config.checkout.enabled = false; config.sub2api_sync.enabled = false
  config.cpa_sync.enabled = false; config.agent_identity_archive.enabled = false
}, { immediate: true })
</script>
