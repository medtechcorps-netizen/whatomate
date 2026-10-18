/** @vitest-environment happy-dom */

import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import ForwardedChip from './ForwardedChip.vue'

function mountChip(state?: 'delayed' | 'uncertain' | null) {
  return mount(ForwardedChip, {
    props: { state },
    global: { mocks: { $t: (key: string) => key } },
  })
}

describe('ForwardedChip', () => {
  it('marks a forwarded bubble for agents', () => {
    const wrapper = mountChip()
    expect(wrapper.text()).toContain('chat.forward.chip')
    expect(wrapper.find('[data-forward-delivery-state]').exists()).toBe(false)
  })

  it('says a delayed forward has an unknown status (never "not sent")', () => {
    const note = mountChip('delayed').get('[data-forward-delivery-state]')
    expect(note.attributes('data-forward-delivery-state')).toBe('delayed')
    expect(note.text()).toBe('chat.forward.delayedNote')
  })

  it('says a forward whose send attempt has no recorded result may have been sent', () => {
    const note = mountChip('uncertain').get('[data-forward-delivery-state]')
    expect(note.text()).toBe('chat.forward.uncertainDeliveryNote')
  })
})
