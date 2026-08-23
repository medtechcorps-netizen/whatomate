/** @vitest-environment happy-dom */

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX } from '@/lib/legacyWhatsAppReplyAttempts'
import { readSelectedOrganizationId } from '@/lib/browserIdentity'

const mocks = vi.hoisted(() => ({
  post: vi.fn(),
  get: vi.fn(),
}))

vi.mock('@/services/api', () => ({
  api: {
    post: mocks.post,
    get: mocks.get,
  },
}))

const user = {
  id: 'user-new',
  email: 'new@example.com',
  full_name: 'New User',
  organization_id: 'organization-new',
}

const oldUser = {
  id: 'user-old',
  email: 'old@example.com',
  full_name: 'Old User',
  organization_id: 'organization-old',
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  const promise = new Promise<T>(res => {
    resolve = res
  })
  return { promise, resolve }
}

function seedOldBrowserIdentity() {
  localStorage.setItem('selected_organization_id', 'organization-old')
  sessionStorage.setItem(
    `${LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX}organization-old:conversation-old`,
    '{"old":"attempt"}',
  )
}

describe('auth store browser identity isolation', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    vi.restoreAllMocks()
    vi.clearAllMocks()
    localStorage.clear()
    sessionStorage.clear()
  })

  it.each(['login', 'register'] as const)(
    'clears inherited organization and pending-reply state before %s',
    async action => {
      seedOldBrowserIdentity()
      mocks.post.mockImplementationOnce(async () => {
        expect(localStorage.getItem('selected_organization_id')).toBeNull()
        expect(
          Array.from({ length: sessionStorage.length }, (_, index) => sessionStorage.key(index))
            .filter(Boolean)
            .some(key => key!.startsWith(LEGACY_WHATSAPP_REPLY_ATTEMPT_PREFIX)),
        ).toBe(false)
        return { data: { data: { user } } }
      })
      const { useAuthStore } = await import('./auth')
      const store = useAuthStore()

      if (action === 'login') {
        await store.login('new@example.com', 'password')
      } else {
        await store.register({
          email: 'new@example.com',
          password: 'password',
          full_name: 'New User',
          invitation_token: 'invitation',
        })
      }

      expect(store.user?.id).toBe('user-new')
      expect(localStorage.getItem('selected_organization_id')).toBeNull()
    },
  )

  it('clears organization-scoped browser state during auth teardown', async () => {
    seedOldBrowserIdentity()
    const { useAuthStore } = await import('./auth')
    const store = useAuthStore()
    store.clearAuth()

    expect(localStorage.getItem('selected_organization_id')).toBeNull()
    expect(sessionStorage.length).toBe(0)
  })

  it('ignores a late /me response after logout and a new identity', async () => {
    const response = deferred<{ data: { data: typeof oldUser } }>()
    mocks.get.mockReturnValue(response.promise)
    const { useAuthStore } = await import('./auth')
    const store = useAuthStore()
    store.setAuth({ user: oldUser })

    const refresh = store.refreshUserData()
    store.clearAuth()
    store.setAuth({ user })
    response.resolve({ data: { data: { ...oldUser, full_name: 'Stale Old Name' } } })

    await expect(refresh).resolves.toBe(false)
    expect(store.user?.id).toBe(user.id)
    expect(store.user?.full_name).toBe(user.full_name)
  })

  it('keeps new-identity entitlements when an old request resolves last', async () => {
    const oldResponse = deferred<{ data: { data: Record<string, unknown> } }>()
    const newResponse = deferred<{ data: { data: Record<string, unknown> } }>()
    mocks.get
      .mockReturnValueOnce(oldResponse.promise)
      .mockReturnValueOnce(newResponse.promise)
    const { useAuthStore } = await import('./auth')
    const store = useAuthStore()
    store.setAuth({ user: oldUser })
    const oldRequest = store.fetchProductEntitlements()

    store.clearAuth()
    store.setAuth({ user })
    const newRequest = store.fetchProductEntitlements()
    newResponse.resolve({
      data: {
        data: {
          entitlements: { 'omnichannel.enabled': true },
          mode: 'licensed',
        },
      },
    })
    await expect(newRequest).resolves.toBe(true)

    oldResponse.resolve({
      data: {
        data: {
          entitlements: { 'old.private.feature': true },
          mode: 'old-tenant',
        },
      },
    })
    await expect(oldRequest).resolves.toBe(false)

    expect(store.productEntitlements).toEqual({ 'omnichannel.enabled': true })
    expect(store.productEntitlementMode).toBe('licensed')
  })

  it('lets only the latest overlapping login establish an identity', async () => {
    const oldLogin = deferred<{ data: { data: { user: typeof oldUser } } }>()
    const newLogin = deferred<{ data: { data: { user: typeof user } } }>()
    mocks.post
      .mockReturnValueOnce(oldLogin.promise)
      .mockReturnValueOnce(newLogin.promise)
    const { useAuthStore } = await import('./auth')
    const store = useAuthStore()

    const first = store.login(oldUser.email, 'old-password')
    const second = store.login(user.email, 'new-password')
    newLogin.resolve({ data: { data: { user } } })
    await second
    oldLogin.resolve({ data: { data: { user: oldUser } } })
    await first

    expect(store.user?.id).toBe(user.id)
    expect(JSON.parse(localStorage.getItem('user') ?? '{}').id).toBe(user.id)
  })

  it('clears in-memory tenant and break state even when localStorage removal throws', async () => {
    const { useAuthStore } = await import('./auth')
    const store = useAuthStore()
    store.setAuth({ user: oldUser })
    store.setAvailability(false, '2026-08-23T10:00:00Z')
    seedOldBrowserIdentity()
    const storagePrototype = Object.getPrototypeOf(localStorage) as Storage
    const originalRemoveItem = storagePrototype.removeItem
    vi.spyOn(storagePrototype, 'removeItem').mockImplementation(function (this: Storage, key: string) {
      if (this === localStorage) {
        throw new DOMException('blocked', 'SecurityError')
      }
      return originalRemoveItem.call(this, key)
    })

    expect(() => store.clearAuth()).not.toThrow()

    expect(store.user).toBeNull()
    expect(store.breakStartedAt).toBeNull()
    expect(store.productEntitlements).toEqual({})
    expect(readSelectedOrganizationId()).toBeNull()
    expect(sessionStorage.length).toBe(0)
  })
})
