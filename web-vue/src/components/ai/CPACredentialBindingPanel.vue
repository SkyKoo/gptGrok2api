<template>
  <section class="space-y-3 rounded-lg border border-border p-3" aria-label="Codex 凭据绑定预览">
    <h3 class="text-sm font-medium">确认绑定已有 CFM 账号</h3>
    <p class="text-xs text-muted-foreground">唯一匹配时预选账号；未选择的条目会跳过。已有不同凭据默认不覆盖，需单独勾选替换。导入只读取 CPA，之后可在 Codex 授权页手动同步。</p>
    <div v-for="(row, index) in rows" :key="row.name" class="space-y-2 border-t border-border pt-3" :aria-label="row.name + ' 绑定'">
      <p class="break-all text-sm">{{ row.email || row.account_id || row.name }}</p>
      <p class="break-all text-xs text-muted-foreground">{{ row.name }}<span v-if="row.expires_at"> · 到期 {{ row.expires_at }}</span></p>
      <p v-if="row.error" class="text-xs text-rose-500">{{ row.error }}</p>
      <template v-else>
        <p class="text-xs text-muted-foreground">Access Token / Refresh Token 完整 · ID Token {{ row.has_id_token ? '已包含' : '未提供' }}</p>
        <input v-model="queries[index]" :disabled="running || finished" class="ui-input-sm w-full" :aria-label="row.name + ' 搜索 CFM 账号'" placeholder="搜索已有账号邮箱或 ID" />
        <select v-model="targets[index]" :disabled="running || finished" class="ui-input-sm w-full" :aria-label="row.name + ' 目标 CFM 账号'" @change="replacements[index] = false">
          <option value="">跳过，不绑定</option>
          <option v-for="candidate in candidates(row, index)" :key="candidate.account_ref" :value="candidate.account_ref" :disabled="Boolean(candidate.conflict)">
            {{ candidate.email || candidate.account_ref }} · {{ candidate.account_ref }}{{ candidate.conflict ? ' · ' + candidate.conflict : candidate.identical ? ' · 相同凭据' : candidate.has_credential ? ' · 已有凭据' : '' }}
          </option>
        </select>
        <p v-if="!row.suggested_ref" class="text-xs text-muted-foreground">未找到唯一匹配，请核对身份后手动选择；没有合适账号可跳过。</p>
        <label v-if="selected(row, index)?.has_credential && !selected(row, index)?.identical" class="flex items-center gap-2 text-xs">
          <input v-model="replacements[index]" type="checkbox" :disabled="running || finished" :aria-label="row.name + ' 替换已有 Codex 凭据'" />替换该账号已有 Codex 凭据
        </label>
      </template>
      <p v-if="results[index]" class="break-all text-xs" :class="results[index].status === 'failed' ? 'text-rose-500' : 'text-muted-foreground'">{{ results[index].message }}</p>
    </div>
    <p v-if="duplicates" class="text-xs text-rose-500">多个文件选择了同一个账号，请调整为一对一绑定。</p>
    <div class="flex flex-wrap items-center justify-between gap-2">
      <p class="text-xs text-muted-foreground" aria-live="polite">{{ running || finished ? `已处理 ${Object.keys(results).length} / ${rows.length}，绑定 ${bound}，跳过 ${skipped}，失败 ${failed}` : `已选 ${chosen} 个绑定目标` }}</p>
      <Button size="xs" variant="primary" :disabled="running || finished || duplicates || chosen === 0" @click="apply">{{ running ? '正在绑定...' : finished ? '本次导入已结束，请重新预览后继续' : '确认导入并绑定' }}</Button>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { Button } from 'nanocat-ui'
import { accountImportsApi, type CPACredentialPreview } from '@/api/accountImports'
import { cpaBindingRequest, duplicateCPATargets } from '@/lib/cpaBinding'
import { useConfirmDialog } from '@/composables/useConfirmDialog'
import { useToast } from '@/composables/useToast'

const props = defineProps<{ poolId: string; rows: CPACredentialPreview[] }>()
const emit = defineEmits<{ 'busy-change': [value: boolean]; imported: [] }>()
const targets = ref(props.rows.map(row => row.suggested_ref || ''))
const queries = ref(props.rows.map(() => ''))
const replacements = ref(props.rows.map(() => false))
const results = ref<Record<number, { status: string; message: string }>>({})
const running = ref(false)
const finished = ref(false)
const toast = useToast()
const confirmDialog = useConfirmDialog()
const chosen = computed(() => targets.value.filter(Boolean).length)
const duplicates = computed(() => duplicateCPATargets(targets.value))
const bound = computed(() => Object.values(results.value).filter(item => item.status === 'bound').length)
const skipped = computed(() => Object.values(results.value).filter(item => item.status === 'skipped').length)
const failed = computed(() => Object.values(results.value).filter(item => item.status === 'failed').length)
function selected(row: CPACredentialPreview, index: number) { return row.candidates.find(item => item.account_ref === targets.value[index]) }
function candidates(row: CPACredentialPreview, index: number) {
  const query = queries.value[index]?.trim().toLowerCase() || ''
  return row.candidates.filter(item => item.account_ref === targets.value[index] || !query || `${item.email} ${item.account_ref}`.toLowerCase().includes(query))
}
async function apply() {
  if (running.value || finished.value || duplicates.value || !chosen.value) return
  const confirmed = await confirmDialog.ask({title:'确认导入 Codex 凭据', message:`将处理 ${chosen.value} 个已有 CFM 账号的绑定，未匹配项跳过；已勾选替换的账号将覆盖原 Codex 凭据。Session、密码、2FA 和分组保持不变。`, confirmText:'导入并绑定', cancelText:'取消'})
  if (!confirmed) return
  running.value = true
  emit('busy-change', true)
  try {
    for (const [index, row] of props.rows.entries()) {
      if (row.error) { results.value[index] = {status:'failed', message:row.error}; continue }
      try {
        const binding = cpaBindingRequest(row, targets.value[index] || '', replacements.value[index] || false)
        if (!binding) { results.value[index] = {status:'skipped', message:'未选择已有账号，已跳过'}; continue }
        results.value[index] = await accountImportsApi.bindCPACredential(props.poolId, binding)
      } catch (error: any) {
        results.value[index] = {status:'failed', message:error.message || '绑定结果未确认，请重新预览核对；不会自动重试'}
      }
    }
    finished.value = true
    toast[failed.value ? 'warning' : 'success'](`Codex 导入：绑定 ${bound.value}，跳过 ${skipped.value}，失败 ${failed.value}`)
    if (bound.value) emit('imported')
  } finally { running.value = false; emit('busy-change', false) }
}
</script>
