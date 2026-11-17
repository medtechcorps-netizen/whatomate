import { getErrorMessage } from './api-utils'

export interface TemplateSyncResult {
  account: string
  success: boolean
  error?: string
}

// Keep each account's existing authorization and avoid a burst of provider calls.
// A session/workspace switch stops the remaining queue before another request.
export async function syncTemplateAccounts(
  accounts: string[],
  sync: (account: string) => Promise<unknown>,
  isCurrent: () => boolean,
  errorFallback = 'Sync failed. Try this account again.',
): Promise<TemplateSyncResult[]> {
  const results: TemplateSyncResult[] = []
  for (const account of new Set(accounts)) {
    if (!isCurrent()) break
    try {
      await sync(account)
      if (!isCurrent()) break
      results.push({ account, success: true })
    } catch (error) {
      if (!isCurrent()) break
      results.push({ account, success: false, error: getErrorMessage(error, errorFallback) })
    }
  }
  return results
}
