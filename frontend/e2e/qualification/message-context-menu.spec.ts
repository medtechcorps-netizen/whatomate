import { expect, test, type Page } from '@playwright/test'

const menu = (page: Page) => page.getByTestId('chat-message-context-menu')
const viewport = (page: Page) => page.locator('[data-reka-scroll-area-viewport]')

async function scriptedPress(page: Page) {
  await page.getByTestId('synthetic-0').dispatchEvent('pointerdown', {
    pointerType: 'touch', pointerId: 7, isPrimary: true, clientX: 70, clientY: 70,
  })
  await expect(menu(page)).toBeVisible()
}

test.beforeEach(async ({ page }) => {
  await page.route('**/*', route => {
    const url = new URL(route.request().url())
    return url.hostname === '127.0.0.1' ? route.continue() : route.abort()
  })
  await page.goto('/e2e/fixtures/message-context-menu.html')
  await expect(page.getByTestId('synthetic-0')).toBeVisible()
})

for (const nativeContext of [false, true]) {
  test(`late selection closes custom menu; subsequent contextmenu=${nativeContext}`, async ({ page }) => {
    await scriptedPress(page)
    const result = await page.getByTestId('synthetic-0').evaluate((element, contextMenu) => {
      const range = document.createRange()
      range.selectNodeContents(element)
      const selection = window.getSelection()!
      selection.removeAllRanges()
      selection.addRange(range)
      if (!contextMenu) document.dispatchEvent(new Event('selectionchange'))
      const event = new PointerEvent('contextmenu', { bubbles: true, cancelable: true, pointerType: 'touch', pointerId: 7 })
      if (contextMenu) element.dispatchEvent(event)
      return { prevented: event.defaultPrevented, text: selection.toString() }
    }, nativeContext)
    expect(result.prevented).toBe(false)
    expect(result.text).toContain('Synthetic message 0')
    await expect(menu(page)).toBeHidden()
    await expect(page.getByTestId('actions')).toHaveText('[]')
  })
}

test('actual ScrollArea viewport scrolling closes the menu and permits a fresh target', async ({ page }) => {
  await scriptedPress(page)
  await viewport(page).evaluate(element => { element.scrollTop = 130 })
  await expect.poll(() => viewport(page).evaluate(element => element.scrollTop)).toBeGreaterThan(0)
  await expect(menu(page)).toBeHidden()
  await viewport(page).evaluate(element => { element.scrollTop = 0 })
  await page.getByTestId('synthetic-1').click({ button: 'right' })
  await page.getByRole('menuitem', { name: 'Reply', exact: true }).click()
  await expect(page.getByTestId('actions')).toHaveText('["reply:synthetic-1"]')
  await expect(page.getByTestId('composer')).toBeFocused()
})

test('native link contextmenu is not cancelled and scope change removes the menu', async ({ page }) => {
  const prevented = await page.getByTestId('link-synthetic-0').evaluate(element => {
    const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true, button: 2 })
    element.dispatchEvent(event)
    return event.defaultPrevented
  })
  expect(prevented).toBe(false)
  await expect(menu(page)).toBeHidden()
  await scriptedPress(page)
  await page.getByTestId('switch-scope').evaluate((element: HTMLButtonElement) => element.click())
  await expect(menu(page)).toBeHidden()
  await expect(page.getByTestId('actions')).toHaveText('[]')
})

for (const holdMs of [650, 1600]) {
  test(`browser-generated touch hold ${holdMs}ms never leaves selection and custom menu together`, async ({ page, context, isMobile }) => {
    test.skip(!isMobile, 'Touch transport is qualified in the touch project; desktop has separate right-click coverage.')
    const session = await context.newCDPSession(page)
    const box = (await page.getByTestId('synthetic-0').boundingBox())!
    await session.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: box.x + 20, y: box.y + 8 }] })
    await page.waitForTimeout(holdMs)
    const state = await page.evaluate(() => ({
      selected: !window.getSelection()!.isCollapsed,
      custom: Boolean(document.querySelector('[data-testid="chat-message-context-menu"]')),
    }))
    expect(state.selected || state.custom).toBe(true)
    expect(state.selected && state.custom).toBe(false)
    await session.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
    await expect(page.getByTestId('actions')).toHaveText('[]')
    await session.detach()
  })
}

test('browser-generated touch scroll moves the viewport without opening a menu', async ({ page, context, isMobile }) => {
  test.skip(!isMobile, 'Touch transport is qualified in the touch project.')
  const session = await context.newCDPSession(page)
  const box = (await viewport(page).boundingBox())!
  const point = { x: box.x + 90, y: box.y + 190 }
  await session.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [point] })
  for (let offset = 20; offset <= 140; offset += 20) {
    await session.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: point.x, y: point.y - offset }] })
  }
  await session.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
  await expect.poll(() => viewport(page).evaluate(element => element.scrollTop)).toBeGreaterThan(0)
  await expect(menu(page)).toBeHidden()
  await expect(page.getByTestId('actions')).toHaveText('[]')
  await session.detach()
})
