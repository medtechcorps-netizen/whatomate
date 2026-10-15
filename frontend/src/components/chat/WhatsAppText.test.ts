/** @vitest-environment happy-dom */

import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import WhatsAppText from './WhatsAppText.vue'

describe('WhatsAppText', () => {
  it('renders WhatsApp formatting as elements', () => {
    const wrapper = mount(WhatsAppText, {
      props: { text: '*Reminder*: take _2 tablets_ ~daily~ `now` ```code```' },
    })
    expect(wrapper.get('strong').text()).toBe('Reminder')
    expect(wrapper.get('em').text()).toBe('2 tablets')
    expect(wrapper.get('s').text()).toBe('daily')
    const codes = wrapper.findAll('code')
    expect(codes.map(code => code.text())).toEqual(['now', 'code'])
    expect(codes[1].attributes('data-wa-mono')).toBeDefined()
    expect(wrapper.text()).toBe('Reminder: take 2 tablets daily now code')
  })

  it('never interprets message text as HTML', () => {
    const payload = '<img src=x onerror="alert(1)"> *<b>bold</b>*'
    const wrapper = mount(WhatsAppText, { props: { text: payload } })
    expect(wrapper.find('img').exists()).toBe(false)
    expect(wrapper.find('b').exists()).toBe(false)
    expect(wrapper.text()).toBe('<img src=x onerror="alert(1)"> <b>bold</b>')
    expect(wrapper.get('strong').text()).toBe('<b>bold</b>')
  })

  it('keeps newlines and literal markers as text', () => {
    const text = '* Panadol\n- 2*3*4 snake_case'
    const wrapper = mount(WhatsAppText, { props: { text } })
    expect(wrapper.element.textContent).toBe(text)
    expect(wrapper.find('strong').exists()).toBe(false)
  })

  it('updates when the text changes', async () => {
    const wrapper = mount(WhatsAppText, { props: { text: 'plain' } })
    expect(wrapper.find('strong').exists()).toBe(false)
    await wrapper.setProps({ text: '*now bold*' })
    expect(wrapper.get('strong').text()).toBe('now bold')
  })
})
