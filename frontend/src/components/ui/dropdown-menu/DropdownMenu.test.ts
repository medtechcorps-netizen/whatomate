/** @vitest-environment happy-dom */

import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, ref } from 'vue'
import { afterEach, describe, expect, it } from 'vitest'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '.'

const mounted: Array<{ unmount: () => void }> = []

function mountMenu(template: string, setup?: () => Record<string, unknown>) {
  const Harness = defineComponent({
    components: { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger },
    setup,
    template,
  })
  const wrapper = mount(Harness, { attachTo: document.body })
  mounted.push(wrapper)
  return wrapper
}

afterEach(() => {
  while (mounted.length) mounted.pop()?.unmount()
  document.body.innerHTML = ''
})

describe('DropdownMenu', () => {
  it('opens an uncontrolled menu when the trigger is clicked', async () => {
    const wrapper = mountMenu(`
      <DropdownMenu>
        <DropdownMenuTrigger as-child>
          <button type="button" data-testid="trigger">More</button>
        </DropdownMenuTrigger>
        <DropdownMenuContent>
          <DropdownMenuItem>First action</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    `)

    const trigger = wrapper.get('[data-testid="trigger"]')
    expect(trigger.attributes('aria-expanded')).toBe('false')

    await trigger.trigger('click', { button: 0 })
    await flushPromises()

    expect(trigger.attributes('aria-expanded')).toBe('true')
    expect(document.body.querySelector('[role="menu"]')?.textContent).toContain('First action')
  })

  it('keeps a controlled menu closed until the parent opens it', async () => {
    const open = ref(false)
    const requested: boolean[] = []
    const wrapper = mountMenu(
      `
      <DropdownMenu :open="open" @update:open="onUpdate">
        <DropdownMenuTrigger as-child>
          <button type="button" data-testid="trigger">More</button>
        </DropdownMenuTrigger>
        <DropdownMenuContent>
          <DropdownMenuItem>Controlled action</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    `,
      () => ({ open, onUpdate: (value: boolean) => requested.push(value) }),
    )

    await wrapper.get('[data-testid="trigger"]').trigger('click', { button: 0 })
    await flushPromises()

    expect(requested).toEqual([true])
    expect(document.body.querySelector('[role="menu"]')).toBeNull()

    open.value = true
    await flushPromises()

    expect(document.body.querySelector('[role="menu"]')?.textContent).toContain('Controlled action')
  })
})
