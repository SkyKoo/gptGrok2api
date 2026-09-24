<template>
  <FormSection v-if="jobs?.length" title="注册任务记录" density="normal">
    <div v-for="job in [...jobs].reverse()" :key="job.id" class="border-b py-3 space-y-1 text-sm">
      <p>{{ job.email || '等待分配邮箱' }} · {{ labels[job.status] || job.status }}</p>
      <p class="text-xs text-muted-foreground">{{ stages[job.stage] || job.stage }} · 入库{{ job.imported ? '完成' : '未完成' }} · 校验{{ job.verified ? '通过' : '未通过' }}</p>
      <p v-if="job.error" class="text-xs break-all">{{ job.error }}</p>
      <Button v-if="job.can_retry" size="sm" variant="outline" :disabled="running || busy" @click="retry(job.id)">重试入库 / 校验</Button>
    </div>
    <p v-if="error" class="text-sm text-red-500">{{ error }}</p>
  </FormSection>
</template>
<script setup lang="ts">
import { ref } from 'vue'
import { Button } from 'nanocat-ui'
import FormSection from '@/components/ai/FormSection.vue'
import { registerApi, type LegacyRegisterConfig } from '@/api/register'
defineProps<{ jobs: LegacyRegisterConfig['jobs']; running: boolean }>()
const emit = defineEmits<{ (e: 'changed'): void }>()
const busy = ref(false), error = ref('')
const labels: Record<string, string> = { queued: '等待执行', running: '进行中', completed: '已完成', failed: '失败', cancelled: '已取消', interrupted: '已中断，需核查', import_pending: '等待入库', verification_pending: '等待校验' }
const stages: Record<string, string> = { queued: '等待执行', mailbox: '创建邮箱别名', authorize: '初始化会话', submit_email: '提交邮箱', send_code: '发送验证码', wait_code: '等待验证码', validate_code: '验证邮箱', create_profile: '创建账号资料', session: '获取会话凭据', import: '导入账号', verify: '校验账号', completed: '账号可用' }
async function retry(id: string) {
  busy.value = true; error.value = ''
  try { await registerApi.retryFreeResult(id); emit('changed') }
  catch (e: any) { error.value = e?.message || '重试失败' }
  finally { busy.value = false }
}
</script>
