/** @vitest-environment happy-dom */

import { mount, shallowMount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import ContactNameSource from './ContactNameSource.vue'
import ContactInfoPanel from './ContactInfoPanel.vue'

vi.mock('vue-i18n', async importOriginal => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({
    t: (key: string, params?: Record<string, string>) =>
      params?.name ? `${key}:${params.name}` : key,
  }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ hasPermission: () => true }),
}))

vi.mock('@/stores/tags', () => ({
  useTagsStore: () => ({ tags: [], fetchTags: vi.fn(), getTagByName: () => undefined }),
}))

function mountSource(contact: Record<string, unknown> | null, displayName: string) {
  return mount(ContactNameSource, { props: { contact: contact as any, displayName } })
}

describe('ContactNameSource', () => {
  it('marks a name saved in WhatsApp Business and shows a differing WhatsApp profile name', () => {
    const wrapper = mountSource(
      { name_source: 'wa_business', wa_profile_name: 'Ali WA' },
      'Ali (Clinic)',
    )
    const badge = wrapper.get('[data-testid="contact-name-source-wa-business"]')
    expect(badge.text()).toBe('contacts.nameSource.savedInWhatsAppBusiness')
    expect(badge.attributes('title')).toBe('contacts.nameSource.savedInWhatsAppBusinessHint')
    expect(wrapper.get('[data-testid="contact-wa-profile-name"]').text()).toBe(
      'contacts.nameSource.whatsappProfile:Ali WA',
    )
  })

  it('says when the saved contact was removed from WhatsApp Business', () => {
    const wrapper = mountSource(
      { name_source: 'wa_business', wa_business_saved_name_removed: true },
      'Ali (Clinic)',
    )
    const badge = wrapper.get('[data-testid="contact-name-source-wa-business"]')
    expect(badge.text()).toBe('contacts.nameSource.removedFromWhatsAppBusiness')
    expect(badge.attributes('title')).toBe('contacts.nameSource.removedFromWhatsAppBusinessHint')
    expect(wrapper.find('[data-testid="contact-wa-profile-name"]').exists()).toBe(false)
  })

  it('shows the WhatsApp profile name under a manual name without a WhatsApp Business marker', () => {
    const wrapper = mountSource({ name_source: 'manual', wa_profile_name: 'Ali WA' }, 'Dr Ali')
    expect(wrapper.find('[data-testid="contact-name-source-wa-business"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="contact-wa-profile-name"]').text()).toBe(
      'contacts.nameSource.whatsappProfile:Ali WA',
    )
  })

  it('renders nothing when the display name is the WhatsApp profile name', () => {
    expect(
      mountSource({ name_source: 'wa_profile', wa_profile_name: ' Ali WA ' }, 'Ali WA')
        .find('[data-testid="contact-name-source"]').exists(),
    ).toBe(false)
    expect(mountSource({}, 'Ali').find('[data-testid="contact-name-source"]').exists()).toBe(false)
    expect(mountSource(null, 'Ali').find('[data-testid="contact-name-source"]').exists()).toBe(false)
  })

  it('aligns to the start for the contact detail card', () => {
    const wrapper = mount(ContactNameSource, {
      props: { contact: { name_source: 'wa_business' }, displayName: 'Ali', align: 'start' },
    })
    expect(wrapper.get('[data-testid="contact-name-source"]').classes()).toContain('items-start')
  })
})

describe('ContactInfoPanel name source', () => {
  const Passthrough = defineComponent({ template: '<div><slot /></div>' })

  it('passes the contact and the shown name to the name-source line', () => {
    const contact = {
      id: 'contact-1',
      phone_number: '+10000000000',
      name: 'Ali (Clinic)',
      profile_name: 'Ali (Clinic)',
      name_source: 'wa_business',
      wa_profile_name: 'Ali WA',
      status: 'active',
      tags: [],
      metadata: {},
      unread_count: 0,
      identity_review_ai_state: {
        known: true,
        ai_allowed: true,
        blocked: false,
        open_hold_count: 0,
        reason: 'no_open_identity_review',
      },
      created_at: '',
      updated_at: '',
    }
    const wrapper = shallowMount(ContactInfoPanel, {
      props: { contact: contact as any, embedded: true },
      global: { stubs: { ScrollArea: Passthrough } },
    })
    expect(wrapper.get('[data-testid="contact-display-name"]').text()).toBe('Ali (Clinic)')
    const source = wrapper.findComponent(ContactNameSource)
    expect(source.exists()).toBe(true)
    expect(source.props('displayName')).toBe('Ali (Clinic)')
    expect(source.props('contact')).toMatchObject({ name_source: 'wa_business', wa_profile_name: 'Ali WA' })
  })
})
