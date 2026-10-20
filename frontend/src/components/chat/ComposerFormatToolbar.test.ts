/** @vitest-environment happy-dom */

import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import ComposerFormatToolbar from './ComposerFormatToolbar.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('ComposerFormatToolbar', () => {
  it('renders four non-submit buttons with bold/italic/strikethrough/code icons', () => {
    const wrapper = mount(ComposerFormatToolbar)
    const buttons = wrapper.findAll('button')
    expect(buttons).toHaveLength(4)
    for (const button of buttons) expect(button.attributes('type')).toBe('button')
    expect(buttons.map(button => button.attributes('data-format'))).toEqual(['bold', 'italic', 'strike', 'mono'])
    expect(wrapper.find('[class*="lucide-bold"]').exists()).toBe(true)
    expect(wrapper.find('[class*="lucide-italic"]').exists()).toBe(true)
    expect(wrapper.find('[class*="lucide-strikethrough"]').exists()).toBe(true)
    expect(wrapper.find('[class*="lucide-code"]').exists()).toBe(true)
    for (const icon of ['lucide-send', 'lucide-paperclip', 'lucide-smile', 'lucide-reply', 'lucide-x']) {
      expect(wrapper.find(`[class*="${icon}"]`).exists(), icon).toBe(false)
    }
    expect(buttons[0].attributes('title')).toMatch(/chat\.composer\.bold \((Ctrl\+|⌘)B\)/)
  })

  it('emits the format kind on click and keeps focus in the textarea', async () => {
    const wrapper = mount(ComposerFormatToolbar)
    const bold = wrapper.get('[data-format="bold"]')
    const mousedown = new MouseEvent('mousedown', { bubbles: true, cancelable: true })
    bold.element.dispatchEvent(mousedown)
    expect(mousedown.defaultPrevented).toBe(true)
    await bold.trigger('click')
    await wrapper.get('[data-format="mono"]').trigger('click')
    expect(wrapper.emitted('format')).toEqual([['bold'], ['mono']])
  })

  it('can be disabled', () => {
    const wrapper = mount(ComposerFormatToolbar, { props: { disabled: true } })
    for (const button of wrapper.findAll('button')) expect(button.attributes('disabled')).toBeDefined()
  })
})
