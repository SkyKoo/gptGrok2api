import type { CPACredentialPreview, CPACredentialBinding } from '../api/accountImports'

export function cpaBindingRequest(row: CPACredentialPreview, accountRef: string, replace: boolean): CPACredentialBinding | null {
  if (!accountRef || row.error) return null
  const target = row.candidates.find(candidate => candidate.account_ref === accountRef)
  if (!target || target.conflict || !row.source_version) throw new Error('绑定关系无效，请重新选择账号或预览')
  return {name: row.name, account_ref: accountRef, source_version: row.source_version, target_version: target.target_version, replace}
}

export function duplicateCPATargets(refs: string[]) {
  const seen = new Set<string>()
  for (const ref of refs) {
    if (!ref) continue
    if (seen.has(ref)) return true
    seen.add(ref)
  }
  return false
}
