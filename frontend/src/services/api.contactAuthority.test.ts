/** @vitest-environment happy-dom */

import type { InternalAxiosRequestConfig } from 'axios'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api, contactsService } from './api'

describe('contact authority request scope', () => {
  const previousAdapter = api.defaults.adapter
  afterEach(() => {
    api.defaults.adapter = previousAdapter
    localStorage.clear()
    vi.restoreAllMocks()
  })

  it('pins a revalidation to its captured organization through the actual interceptor', async () => {
    const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ({
      data: { data: { id: 'contact' } }, status: 200, statusText: 'OK', headers: {}, config,
    }))
    api.defaults.adapter = adapter
    localStorage.setItem('selected_organization_id', 'new-workspace')
    await contactsService.get('contact', 'captured-org')
    expect(adapter).toHaveBeenCalledTimes(1)
    expect(adapter.mock.calls[0]![0].url).toBe('/contacts/contact')
    expect(adapter.mock.calls[0]![0].headers.get('X-Organization-ID')).toBe('captured-org')
  })

  it('keeps the ordinary contact request compatible when scope is not supplied', async () => {
    const get = vi.spyOn(api, 'get').mockResolvedValue({ data: {} })
    await contactsService.get('contact')
    expect(get).toHaveBeenCalledWith('/contacts/contact', undefined)
  })
})
