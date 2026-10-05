/** @vitest-environment happy-dom */

import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import StagedIdentityReviewNotice from './StagedIdentityReviewNotice.vue'

const mocks = vi.hoisted(() => ({
  listStaged: vi.fn(),
}))

vi.mock('@/services/api', async importOriginal => ({
  ...(await importOriginal<typeof import('@/services/api')>()),
  contactsService: {
    listStagedIdentityReviews: mocks.listStaged,
  },
}))

vi.mock('./ContactIdentityReviewDialog.vue', async () => {
  const { defineComponent: define } = await import('vue')
  return {
    default: define({
      name: 'ContactIdentityReviewDialog',
      props: {
        open: Boolean,
        contactId: { type: String, default: null },
        canReview: Boolean,
        canViewStaged: Boolean,
        initialSection: { type: String, default: undefined },
      },
      emits: ['update:open', 'staged-total'],
      template: '<div data-testid="dialog-stub" />',
    }),
  }
})

const DialogName = defineComponent({ name: 'ContactIdentityReviewDialog' }).name

function stagedResponse(total: number, readOnlyTotal?: number) {
  return { data: { data: { reviews: [], total, read_only_total: readOnlyTotal } } }
}

function mountNotice(props: Record<string, unknown> = {}) {
  return mount(StagedIdentityReviewNotice, {
    props: { canView: true, refreshIntervalMs: 0, ...props },
    global: {
      stubs: {
        Button: { template: '<button><slot /></button>' },
      },
    },
  })
}

describe('StagedIdentityReviewNotice', () => {
  let wrapper: VueWrapper | null = null

  beforeEach(() => {
    mocks.listStaged.mockReset()
  })

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
  })

  it('never requests the protected queue without review authority', async () => {
    wrapper = mountNotice({ canView: false })
    await flushPromises()

    expect(mocks.listStaged).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="staged-identity-review-notice"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="dialog-stub"]').exists()).toBe(false)
  })

  it('stays hidden when nothing is held or the count cannot be read', async () => {
    mocks.listStaged.mockResolvedValueOnce(stagedResponse(0))
    wrapper = mountNotice()
    await flushPromises()
    expect(mocks.listStaged).toHaveBeenCalledWith({ page: 1, limit: 1 }, expect.any(AbortSignal))
    expect(wrapper.find('[data-testid="staged-identity-review-notice"]').exists()).toBe(false)
    wrapper.unmount()

    mocks.listStaged.mockRejectedValueOnce(new Error('forbidden'))
    wrapper = mountNotice()
    await flushPromises()
    expect(wrapper.find('[data-testid="staged-identity-review-notice"]').exists()).toBe(false)
  })

  it('shows the held count and opens the queue without a contact', async () => {
    mocks.listStaged.mockResolvedValue(stagedResponse(2))
    wrapper = mountNotice()
    await flushPromises()

    expect(wrapper.get('[data-testid="staged-identity-review-count"]').text()).toBe('2')
    expect(wrapper.text()).toContain('WhatsApp messages are held for identity review')
    expect(wrapper.findComponent({ name: DialogName }).exists()).toBe(false)

    await wrapper.get('[data-testid="staged-identity-review-open"]').trigger('click')
    const dialog = wrapper.findComponent({ name: DialogName })
    expect(dialog.props('open')).toBe(true)
    expect(dialog.props('contactId')).toBeNull()
    expect(dialog.props('initialSection')).toBe('staged')
    expect(dialog.props('canViewStaged')).toBe(true)

    dialog.vm.$emit('staged-total', 1)
    await flushPromises()
    expect(wrapper.get('[data-testid="staged-identity-review-count"]').text()).toBe('1')
    expect(wrapper.text()).toContain('WhatsApp message is held for identity review')

    mocks.listStaged.mockResolvedValueOnce(stagedResponse(0))
    dialog.vm.$emit('update:open', false)
    await flushPromises()
    expect(mocks.listStaged).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid="staged-identity-review-notice"]').exists()).toBe(false)
  })

  it('says which held messages no decision can resolve', async () => {
    mocks.listStaged.mockResolvedValueOnce(stagedResponse(1, 1))
    wrapper = mountNotice()
    await flushPromises()
    expect(wrapper.get('[data-testid="staged-identity-review-notice"]').text())
      .toContain('1 WhatsApp message is held for identity review · read-only for now')
    wrapper.unmount()

    mocks.listStaged.mockResolvedValueOnce(stagedResponse(3, 2))
    wrapper = mountNotice()
    await flushPromises()
    expect(wrapper.get('[data-testid="staged-identity-review-read-only"]').text()).toBe('· 2 read-only for now')
    wrapper.unmount()

    mocks.listStaged.mockResolvedValueOnce(stagedResponse(4, 4))
    wrapper = mountNotice()
    await flushPromises()
    expect(wrapper.get('[data-testid="staged-identity-review-read-only"]').text()).toBe('· all read-only for now')
    wrapper.unmount()

    // A server without the field, or a decidable queue, shows no marker.
    mocks.listStaged.mockResolvedValueOnce(stagedResponse(2))
    wrapper = mountNotice()
    await flushPromises()
    expect(wrapper.get('[data-testid="staged-identity-review-count"]').text()).toBe('2')
    expect(wrapper.find('[data-testid="staged-identity-review-read-only"]').exists()).toBe(false)
  })

  it('refreshes on an interval while visible', async () => {
    vi.useFakeTimers()
    try {
      mocks.listStaged.mockResolvedValue(stagedResponse(0))
      wrapper = mountNotice({ refreshIntervalMs: 1000 })
      await flushPromises()
      expect(mocks.listStaged).toHaveBeenCalledTimes(1)
      mocks.listStaged.mockResolvedValue(stagedResponse(3))
      await vi.advanceTimersByTimeAsync(1000)
      await flushPromises()
      expect(mocks.listStaged).toHaveBeenCalledTimes(2)
      expect(wrapper.get('[data-testid="staged-identity-review-count"]').text()).toBe('3')
    } finally {
      vi.useRealTimers()
    }
  })
})
