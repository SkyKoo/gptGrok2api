export type OAuthProviderKey = 'codex' | 'xai'

export type OAuthProviderDefinition = {
  key: OAuthProviderKey
  label: string
  description: string
}

// Keep provider metadata in one registry so adding another OAuth provider only
// requires a new adapter/view and one entry here.
export const oauthProviderDefinitions: readonly OAuthProviderDefinition[] = [
  {
    key: 'codex',
    label: 'Codex OAuth',
    description: '管理 ChatGPT Codex OAuth 凭据与 CPA 同步。',
  },
  {
    key: 'xai',
    label: 'xAI OAuth',
    description: '管理 Grok/xAI OAuth 凭据、模型探测与授权方式。',
  },
]

export function normalizeOAuthProvider(value: unknown): OAuthProviderKey {
  return value === 'xai' ? 'xai' : 'codex'
}
