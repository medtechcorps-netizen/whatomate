/** @vitest-environment happy-dom */

import type { InternalAxiosRequestConfig } from 'axios'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, contactsService, usersService } from './api'

describe('assignment directory requests', () => {
  const previousAdapter = api.defaults.adapter
  afterEach(() => {
    api.defaults.adapter = previousAdapter
    localStorage.removeItem('selected_organization_id')
    vi.restoreAllMocks()
  })

  for (const source of ['members', 'users'] as const) {
    it(`pins the ${source} page and search to the captured organization through the interceptor`, async () => {
      const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ({
        data: { data: { [source]: [], total: 0, page: 2, limit: 100 } },
        status: 200, statusText: 'OK', headers: {}, config,
      }))
      api.defaults.adapter = adapter
      localStorage.setItem('selected_organization_id', 'different-new-workspace')
      const signal = new AbortController().signal
      const params = { page: 2, limit: 100, search: 'Person beyond page one' }
      if (source === 'members') await contactsService.assignableMembers(params, signal, 'captured-org')
      else await usersService.list(params, signal, 'captured-org')
      expect(adapter).toHaveBeenCalledTimes(1)
      const config = adapter.mock.calls[0][0]
      expect(config.url).toBe(source === 'members' ? '/organizations/members' : '/users')
      expect(config.params).toEqual(params)
      expect(config.signal).toBe(signal)
      expect(config.headers.get('X-Organization-ID')).toBe('captured-org')
    })
  }

  it('preserves the ordinary settings users request without an explicit organization', async () => {
    const get = vi.spyOn(api, 'get').mockResolvedValue({ data: {} })
    await usersService.list({ page: 2, limit: 20 })
    expect(get).toHaveBeenCalledWith('/users', { params: { page: 2, limit: 20 }, signal: undefined })
  })
})
