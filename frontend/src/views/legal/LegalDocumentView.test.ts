/** @vitest-environment happy-dom */

import { mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it } from 'vitest'
import LegalDocumentView from './LegalDocumentView.vue'

describe.each(['/', '/console/'])('legal section navigation at %s', basePath => {
  it.each(['privacy', 'terms', 'data-deletion'] as const)('keeps %s anchors on the current document', async documentKey => {
    const base = document.createElement('base')
    base.href = basePath
    document.head.prepend(base)
    const router = createRouter({
      history: createMemoryHistory(basePath),
      routes: [{ path: '/:documentKey', component: LegalDocumentView, props: true }],
    })
    await router.push(`/${documentKey}`)
    await router.isReady()
    const wrapper = mount(LegalDocumentView, {
      props: { documentKey },
      global: { plugins: [router] },
      attachTo: document.body,
    })
    try {
      const links = wrapper.findAll('nav[aria-label="Document sections"] a')
      expect(links.length).toBeGreaterThan(0)
      for (const link of links) {
        const target = new URL((link.element as HTMLAnchorElement).href)
        expect(target.pathname).toBe(`${basePath}${documentKey}`)
        expect(target.hash).not.toBe('')
        expect(wrapper.find(target.hash).exists()).toBe(true)
      }
    } finally {
      wrapper.unmount()
      base.remove()
    }
  })
})
