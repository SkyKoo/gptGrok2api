<template>
  <div class="flex items-center gap-2" :class="alignClass">
    <Button
      size="xs"
      variant="outline"
      root-class="min-w-24 justify-center whitespace-nowrap"
      title="同步已保存的 SSO 登录态及配置，使账号可供文字、图片和视频请求调度；异常或禁用账号仍不会被选用"
      :disabled="busy || !item.has_sso || !runtimeAvailable || syncing"
      @click="emit('sync')"
    >
      {{ syncing ? '同步中' : (item.sync_state === 'synced' ? '同步调用池' : '加入调用池') }}
    </Button>
    <Button
      size="xs"
      variant="outline"
      root-class="w-16 justify-center whitespace-nowrap"
      :disabled="busy || !item.has_sso || testing"
      @click="emit('test')"
    >
      {{ testing ? '测试中' : '测试' }}
    </Button>
    <Button
      size="xs"
      variant="outline"
      root-class="min-w-16 justify-center whitespace-nowrap"
      :disabled="busy || reloginActive || !item.has_password || !item.email || isRuntimeDisabled || item.status === 'disabled'"
      @click="emit('relogin')"
    >
      {{ reloginActive ? reloginLabel : '重新登录' }}
    </Button>
    <span v-if="!reloginActive && reloginLabel" class="whitespace-nowrap text-xs text-muted-foreground" :title="reloginDetail" aria-live="polite">
      {{ reloginLabel }}
    </span>
    <FloatingActionMenu
      label="更多"
      :items="menuItems"
      :disabled="busy || syncing || refreshing || testing || toggling || deleting"
      align="right"
      size="sm"
      trigger-class="h-7 justify-center px-2 text-[11px]"
      :trigger-width="64"
      @select="handleSelect"
    />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { Button } from 'nanocat-ui'
import type { ActionMenuItem } from 'nanocat-ui'

import type { GrokAccount, GrokReloginJob } from '@/api/grokAccounts'
import type { GrokOAuthAccount } from '@/api/grokOAuthAccounts'
import FloatingActionMenu from './FloatingActionMenu.vue'
import { actionMenuGroups } from './menuItems'

const props = withDefaults(defineProps<{
  item: GrokAccount
  runtimeAvailable?: boolean
  busy?: boolean
  syncing?: boolean
  refreshing?: boolean
  testing?: boolean
  relogin?: GrokReloginJob
  startingRelogin?: boolean
  toggling?: boolean
  deleting?: boolean
  authorizing?: boolean
  oauthAccount?: GrokOAuthAccount | null
  oauthAction?: string
  align?: 'start' | 'end'
}>(), {
  runtimeAvailable: false,
  busy: false,
  syncing: false,
  refreshing: false,
  testing: false,
  toggling: false,
  deleting: false,
  authorizing: false,
  oauthAccount: null,
  oauthAction: '',
  align: 'start',
})

const emit = defineEmits<{
  (e: 'credentials'): void
  (e: 'sync'): void
  (e: 'refresh'): void
  (e: 'test'): void
  (e: 'relogin'): void
  (e: 'toggle-disabled'): void
  (e: 'remove'): void
  (e: 'oauth-sync'): void
  (e: 'oauth-authorize'): void
  (e: 'oauth-refresh'): void
  (e: 'oauth-toggle'): void
  (e: 'oauth-remove'): void
}>()

const alignClass = computed(() => props.align === 'end' ? 'justify-end' : 'justify-start')
const isRuntimeDisabled = computed(() => String(props.item.runtime_status || '').toLowerCase() === 'disabled')
const canManageRuntime = computed(() => props.runtimeAvailable && props.item.sync_state === 'synced')
const oauthActionBusy = computed(() => Boolean(props.oauthAction))
const reloginActive = computed(() => props.startingRelogin || props.relogin?.status === 'queued' || props.relogin?.status === 'running')
const reloginDetail = computed(() => {
  const job = props.relogin
  if (job?.status === 'completed') {
    const quotas = [
      ['文字', job.result?.text_quota], ['图片', job.result?.image_quota], ['视频', job.result?.video_quota],
    ]
    const missing = quotas.filter(([, refreshed]) => !refreshed).map(([label]) => label)
    return missing.length ? '额度尚未确认：' + missing.join('、') : '文字、图片和视频额度已刷新'
  }
  const details: Record<string, string> = {
    grok_relogin_verification_required: 'Grok 要求额外验证，请在官网完成',
    grok_relogin_login_page_unrecognized: '未识别当前 Grok 登录页面',
    grok_relogin_login_timeout: '等待 Grok 登录完成超时',
    grok_relogin_identity_mismatch: '新会话与所选账号身份不一致',
    grok_relogin_proxy_unavailable: '账号绑定的代理出口不可用',
    grok_relogin_browser_unavailable: '共享浏览器暂不可用',
    grok_relogin_account_busy: '账号正在运行请求或等待额度结算',
    grok_relogin_account_changed: '账号或登录凭据已变化',
    grok_relogin_sync_failed: '调用池同步未完成，再次点击将继续同步已保存的会话',
    grok_relogin_interrupted: '任务中断，请核对登录状态后重试',
    grok_relogin_result_unknown: '浏览器登录结果未知',
  }
  return (job?.error && details[job.error]) || ''
})
const reloginLabel = computed(() => {
  if (props.startingRelogin) return '正在提交'
  const job = props.relogin
  if (job?.status === 'completed') {
    return job.result?.text_quota && job.result.image_quota && job.result.video_quota ? '已重新登录' : '已登录，待确认额度'
  }
  if (job?.status === 'failed' || job?.status === 'cancelled') return '重新登录未完成'
  if (job?.status === 'queued') return '等待浏览器'
  if (job?.status !== 'running') return ''
  const stages: Record<string, string> = {
    waiting_browser: '等待浏览器',
    starting_browser: '启动浏览器',
    signing_in: '正在登录',
    syncing: '同步调用池',
    refreshing_quota: '刷新额度',
  }
  return stages[job.result?.stage || ''] || '重新登录中'
})

const menuItems = computed<ActionMenuItem[]>(() => actionMenuGroups(
  [
    {
      key: 'credentials',
      label: '登录凭据',
      disabled: !props.item.has_password,
    },
    {
      key: 'refresh',
      label: props.refreshing ? '刷新中...' : '刷新状态和额度',
      disabled: !canManageRuntime.value || props.refreshing,
    },
    {
      key: 'toggle-disabled',
      label: props.toggling ? '处理中...' : (isRuntimeDisabled.value ? '恢复账号' : '禁用账号'),
      disabled: !canManageRuntime.value || props.toggling,
    },
  ],
  [
    {
      key: 'oauth-authorize',
      label: 'OAuth 授权与 CPA',
      disabled: oauthActionBusy.value || props.authorizing,
    },
  ],
  [
    {
      key: 'remove',
      label: props.deleting ? '删除中...' : '删除账号',
      disabled: props.deleting,
      danger: true,
    },
  ],
))

function handleSelect(key: string) {
  if (key === 'credentials') emit('credentials')
  if (key === 'refresh') emit('refresh')
  if (key === 'toggle-disabled') emit('toggle-disabled')
  if (key === 'remove') emit('remove')
  if (key === 'oauth-sync') emit('oauth-sync')
  if (key === 'oauth-authorize') emit('oauth-authorize')
  if (key === 'oauth-refresh') emit('oauth-refresh')
  if (key === 'oauth-toggle') emit('oauth-toggle')
  if (key === 'oauth-remove') emit('oauth-remove')
}
</script>
