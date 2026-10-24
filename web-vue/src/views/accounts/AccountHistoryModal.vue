<template>
  <ModalShell :open="open" max-width="64rem" :z-index="150">
    <ModalHeader
      title="账号历史"
      subtitle="查看注册、添加和删除记录。历史只保存账号标识，不保存访问令牌或密码。"
      compact
      @close="emit('close')"
    />
    <ModalBody density="compact" class="space-y-3">
      <div class="flex flex-wrap items-center gap-2">
        <Input v-model="keyword" block root-class="min-w-[14rem] flex-1" placeholder="搜索邮箱 / 账号 ID / 来源" @keyup.enter="load" />
        <select v-model="action" class="h-9 rounded-md border border-border bg-background px-2 text-sm text-foreground" aria-label="历史动作">
          <option value="">全部动作</option>
          <option value="registered">注册</option>
          <option value="added">添加</option>
          <option value="deleted">删除</option>
        </select>
        <Button size="sm" variant="outline" :disabled="loading" @click="load">
          {{ loading ? '加载中...' : '查询' }}
        </Button>
      </div>

      <div v-if="error" class="rounded-md border border-rose-500/30 bg-rose-500/5 px-3 py-2 text-sm text-rose-600">{{ error }}</div>
      <div v-else-if="!loading && !items.length" class="rounded-md border border-border/70 px-3 py-8 text-center text-sm text-muted-foreground">
        暂无账号历史记录
      </div>
      <div v-else class="overflow-x-auto rounded-md border border-border/70">
        <table class="min-w-[760px] w-full text-left text-sm">
          <thead class="bg-muted/30 text-xs text-muted-foreground">
            <tr>
              <th class="px-3 py-2">时间</th>
              <th class="px-3 py-2">动作</th>
              <th class="px-3 py-2">账号</th>
              <th class="px-3 py-2">来源 / 类型</th>
              <th class="px-3 py-2">状态</th>
              <th class="px-3 py-2">注册时间</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in items" :key="item.id" class="border-t border-border/60">
              <td class="px-3 py-2 whitespace-nowrap text-muted-foreground">{{ formatDate(item.event_at) }}</td>
              <td class="px-3 py-2 whitespace-nowrap">{{ actionLabel(item.action) }}</td>
              <td class="px-3 py-2">
                <div class="font-medium">{{ item.email || item.account_ref }}</div>
                <div v-if="item.email" class="font-mono text-[11px] text-muted-foreground">{{ item.account_ref }}</div>
                <div v-if="item.user_id" class="text-[11px] text-muted-foreground">{{ item.user_id }}</div>
              </td>
              <td class="px-3 py-2 text-muted-foreground">{{ item.source_type || '-' }}<span v-if="item.type"> / {{ item.type }}</span></td>
              <td class="px-3 py-2">{{ item.status || '-' }}</td>
              <td class="px-3 py-2 whitespace-nowrap text-muted-foreground">{{ formatDate(item.created_at) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div v-if="total > 0" class="flex items-center justify-between text-xs text-muted-foreground">
        <span>共 {{ total }} 条</span>
        <div class="flex items-center gap-2">
          <Button size="xs" variant="outline" :disabled="loading || page <= 1" @click="page--; load()">上一页</Button>
          <span>{{ page }} / {{ pageCount }}</span>
          <Button size="xs" variant="outline" :disabled="loading || page >= pageCount" @click="page++; load()">下一页</Button>
        </div>
      </div>
    </ModalBody>
  </ModalShell>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { Button, Input } from 'nanocat-ui'
import { accountsApi, type AccountHistoryItem } from '@/api/accounts'
import ModalBody from '@/components/ai/ModalBody.vue'
import ModalHeader from '@/components/ai/ModalHeader.vue'
import ModalShell from '@/components/ai/ModalShell.vue'

const props = defineProps<{ open: boolean }>()
const emit = defineEmits<{ (event: 'close'): void }>()
const items = ref<AccountHistoryItem[]>([])
const keyword = ref('')
const action = ref('')
const page = ref(1)
const pageSize = 20
const total = ref(0)
const loading = ref(false)
const error = ref('')
const pageCount = computed(() => Math.max(1, Math.ceil(total.value / pageSize)))

function actionLabel(value: string) {
  return ({ registered: '注册', added: '添加', deleted: '删除' } as Record<string, string>)[value] || value || '-'
}

function formatDate(value?: string) {
  if (!value) return '-'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const response = await accountsApi.listHistory({
      page: page.value,
      page_size: pageSize,
      action: action.value || undefined,
      keyword: keyword.value.trim() || undefined,
    })
    items.value = response.items || []
    total.value = Number(response.total || 0)
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : '读取账号历史失败'
  } finally {
    loading.value = false
  }
}

// The modal is opened from the account page; load lazily whenever it becomes visible.
watch(() => props.open, (visible) => {
  if (visible) {
    page.value = 1
    void load()
  }
})
</script>
