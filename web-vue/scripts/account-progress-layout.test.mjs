import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import test from 'node:test'

const read = (name) => readFile(new URL(`../src/views/accounts/${name}`, import.meta.url), 'utf8')

test('2FA and relogin progress panels start collapsed and keep a bounded scroll area', async () => {
  const [twoFA, relogin, twoFAPanel, reloginPanel] = await Promise.all([
    read('accountTwoFARuntime.ts'),
    read('accountReloginRuntime.ts'),
    read('AccountTwoFAPanel.vue'),
    read('AccountReloginPanel.vue'),
  ])

  assert.match(twoFA, /const expanded = ref\(false\)/)
  assert.match(relogin, /const expanded = ref\(false\)/)
  assert.doesNotMatch(twoFA, /async function start\(accountId: string\) \{[\s\S]*?showProgress\(\)/)
  assert.doesNotMatch(relogin, /async function start\(accountId: string\) \{[\s\S]*?showProgress\(\)/)
  assert.match(twoFAPanel, /max-h-\[28rem\] overflow-y-auto/)
  assert.match(reloginPanel, /max-h-\[28rem\] overflow-y-auto/)
})
