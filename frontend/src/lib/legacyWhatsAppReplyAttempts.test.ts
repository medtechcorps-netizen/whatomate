/** @vitest-environment happy-dom */

import { beforeEach, describe, expect, it, vi } from 'vitest'

const input = {
  organizationId: 'organization-1',
  conversationId: 'conversation-1',
  body: 'Exact UTF-8 body – 私人',
  serviceWindowEndsAt: '2099-01-01T00:00:00.000Z',
}

async function loadAttemptsModule() {
  vi.resetModules()
  return import('./legacyWhatsAppReplyAttempts')
}

function storageWith(overrides: Partial<Storage>): Storage {
  const actual = window.sessionStorage
  return {
    get length() {
      return actual.length
    },
    clear: () => actual.clear(),
    getItem: key => actual.getItem(key),
    key: index => actual.key(index),
    removeItem: key => actual.removeItem(key),
    setItem: (key, value) => actual.setItem(key, value),
    ...overrides,
  }
}

describe('legacy WhatsApp reply attempt storage', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
    sessionStorage.clear()
  })

  it('reuses the persisted key after a real module reload and beyond fifteen minutes', async () => {
    vi.spyOn(crypto, 'randomUUID')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000061')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000062')
    const firstModule = await loadAttemptsModule()
    const createdAt = Date.now()
    const first = await firstModule.getOrCreateLegacyWhatsAppReplyAttempt({
      ...input,
      now: createdAt,
    })

    // resetModules removes every in-memory module variable. sessionStorage is
    // therefore the only mechanism capable of preserving this logical send.
    const reloadedModule = await loadAttemptsModule()
    const retry = await reloadedModule.getOrCreateLegacyWhatsAppReplyAttempt({
      ...input,
      now: createdAt + 16 * 60 * 1000,
    })

    expect(retry.key).toBe(first.key)
    expect(retry.reused).toBe(true)
    expect(crypto.randomUUID).toHaveBeenCalledTimes(1)
  })

  it('mints a new key for the same body when a later inbound opens a new service window', async () => {
    vi.spyOn(crypto, 'randomUUID')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000071')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000072')
    const firstModule = await loadAttemptsModule()
    const firstWindowStart = Date.parse('2026-08-23T00:00:00.000Z')
    const first = await firstModule.getOrCreateLegacyWhatsAppReplyAttempt({
      ...input,
      now: firstWindowStart,
      serviceWindowEndsAt: '2026-08-23T01:00:00.000Z',
    })

    const reloadedModule = await loadAttemptsModule()
    const later = await reloadedModule.getOrCreateLegacyWhatsAppReplyAttempt({
      ...input,
      now: Date.parse('2026-08-23T02:00:00.000Z'),
      serviceWindowEndsAt: '2026-08-24T02:00:00.000Z',
    })

    expect(first.key).toBe('00000000-0000-4000-8000-000000000071')
    expect(later.key).toBe('00000000-0000-4000-8000-000000000072')
    expect(later.reused).toBe(false)
  })

  it('reuses the ambiguous key when the window is extended before its original expiry', async () => {
    vi.spyOn(crypto, 'randomUUID')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000081')
      .mockReturnValueOnce('00000000-0000-4000-8000-000000000082')
    const module = await loadAttemptsModule()
    const first = await module.getOrCreateLegacyWhatsAppReplyAttempt({
      ...input,
      now: Date.parse('2026-08-23T00:00:00.000Z'),
      serviceWindowEndsAt: '2026-08-23T01:00:00.000Z',
    })
    const extended = await module.getOrCreateLegacyWhatsAppReplyAttempt({
      ...input,
      now: Date.parse('2026-08-23T00:30:00.000Z'),
      serviceWindowEndsAt: '2026-08-24T00:30:00.000Z',
    })

    expect(extended.key).toBe(first.key)
    expect(extended.reused).toBe(true)
    expect(crypto.randomUUID).toHaveBeenCalledTimes(1)
  })

  it('rejects malformed window metadata without discarding the unresolved key', async () => {
    const module = await loadAttemptsModule()
    const first = await module.getOrCreateLegacyWhatsAppReplyAttempt(input)
    const storedBefore = Array.from(
      { length: sessionStorage.length },
      (_, index) => sessionStorage.key(index),
    ).find(key => key?.includes('organization-1:conversation-1'))

    await expect(
      module.getOrCreateLegacyWhatsAppReplyAttempt({
        ...input,
        serviceWindowEndsAt: 'not-a-service-window',
      }),
    ).rejects.toThrow('WhatsApp service window is not available')

    expect(storedBefore).toBeDefined()
    expect(JSON.parse(sessionStorage.getItem(storedBefore!) ?? '{}').key).toBe(
      first.key,
    )
  })

  it('fails closed when the persisted service-window identity is malformed', async () => {
    const module = await loadAttemptsModule()
    await module.getOrCreateLegacyWhatsAppReplyAttempt(input)
    const storedKey = Array.from(
      { length: sessionStorage.length },
      (_, index) => sessionStorage.key(index),
    ).find(key => key?.includes('organization-1:conversation-1'))
    const record = JSON.parse(sessionStorage.getItem(storedKey!) ?? '{}')
    record.serviceWindowEndsAt = 'corrupted-window'
    sessionStorage.setItem(storedKey!, JSON.stringify(record))

    await expect(
      module.getOrCreateLegacyWhatsAppReplyAttempt(input),
    ).rejects.toBeInstanceOf(module.LegacyWhatsAppReplyAttemptStorageError)
  })

  it.each(['getItem', 'setItem'] as const)(
    'fails closed when sessionStorage.%s throws',
    async method => {
      const module = await loadAttemptsModule()
      const sessionStorageGetter = vi
        .spyOn(window, 'sessionStorage', 'get')
        .mockReturnValue(
          storageWith({
            [method]: () => {
              throw new DOMException('blocked', 'SecurityError')
            },
          }),
        )

      try {
        await expect(
          module.getOrCreateLegacyWhatsAppReplyAttempt(input),
        ).rejects.toBeInstanceOf(module.LegacyWhatsAppReplyAttemptStorageError)
      } finally {
        sessionStorageGetter.mockRestore()
      }
    },
  )

  it('fails closed when removal cannot be verified before replacing a draft', async () => {
    const module = await loadAttemptsModule()
    await module.getOrCreateLegacyWhatsAppReplyAttempt(input)
    const sessionStorageGetter = vi
      .spyOn(window, 'sessionStorage', 'get')
      .mockReturnValue(storageWith({ removeItem: () => undefined }))

    try {
      await expect(
        module.getOrCreateLegacyWhatsAppReplyAttempt({
          ...input,
          body: 'Edited body',
        }),
      ).rejects.toBeInstanceOf(module.LegacyWhatsAppReplyAttemptStorageError)
    } finally {
      sessionStorageGetter.mockRestore()
    }
  })

  it('keeps identity cleanup nonthrowing and blocks sends until cleanup verifies', async () => {
    const module = await loadAttemptsModule()
    await module.getOrCreateLegacyWhatsAppReplyAttempt(input)
    const sessionStorageGetter = vi
      .spyOn(window, 'sessionStorage', 'get')
      .mockReturnValue(storageWith({ removeItem: () => undefined }))

    expect(() => module.clearLegacyWhatsAppReplyAttemptNamespace()).not.toThrow()
    await expect(
      module.getOrCreateLegacyWhatsAppReplyAttempt(input),
    ).rejects.toBeInstanceOf(module.LegacyWhatsAppReplyAttemptStorageError)

    sessionStorageGetter.mockRestore()
    module.clearLegacyWhatsAppReplyAttemptNamespace()
    await expect(
      module.getOrCreateLegacyWhatsAppReplyAttempt(input),
    ).resolves.toMatchObject({ reused: false })
  })
})
