import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'
import { expect, test, type Locator, type Page, type TestInfo } from '@playwright/test'
import postcss from 'postcss'
import tailwindcss, { type Config } from 'tailwindcss'

const require = createRequire(import.meta.url)
const frontendURL = new URL('../../../', import.meta.url)
const projectConfig = require(fileURLToPath(new URL('tailwind.config.cjs', frontendURL))) as Config
const digest = (bytes: string | Buffer) => createHash('sha256').update(bytes).digest('hex')

// Raw content is deliberately outside the application content glob. Every
// state declaration below must come from the locked compiler, never substitute
// handwritten group/peer CSS or inject it into the built About document.
const markup = `<!doctype html><html><head><meta charset="utf-8"></head><body>
<main style="padding:24px;display:grid;gap:18px;width:600px">
  <button id="ordinary-hover" class="text-slate-900 hover:text-green-600">Ordinary hover</button>
  <button id="ordinary-focus" class="focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-red-500">Ordinary focus</button>
  <button id="after-ordinary">After ordinary</button>
  <section id="group-a" class="group group/panel" style="padding:12px">
    <span id="group-target" class="inline-block opacity-40 group-hover:opacity-100 group-hover:translate-x-1">Group target</span>
    <span id="named-target" class="inline-block group-hover/panel:translate-x-2">Named target</span>
  </section>
  <section id="group-b" class="group group/panel" style="padding:12px">
    <span id="group-control" class="inline-block opacity-40 group-hover:opacity-100 group-hover/panel:translate-x-2">Unrelated group</span>
  </section>
  <button id="before-within">Before focus group</button>
  <section class="group">
    <button id="within-button">Inside focus group</button>
    <span id="within-target" class="opacity-40 group-focus-within:opacity-100">Focus target</span>
  </section>
  <button id="after-within">After focus group</button>
  <section>
    <button id="before-peer">Before peer</button>
    <input id="peer-a" type="checkbox" class="peer/picker" aria-label="First peer">
    <span id="peer-checked" class="text-slate-900 peer-checked/picker:text-green-600">Checked target</span>
    <span id="peer-focus" class="opacity-40 peer-focus-visible/picker:opacity-100">Focus target</span>
    <span id="peer-disabled" class="opacity-100 cursor-default peer-disabled/picker:opacity-25 peer-disabled/picker:cursor-not-allowed">Disabled target</span>
  </section>
  <section>
    <input id="peer-b" type="checkbox" class="peer/picker" aria-label="Independent peer">
    <span id="peer-control" class="text-slate-900 peer-checked/picker:text-green-600">Independent target</span>
  </section>
  <section>
    <input id="ordinary-peer" type="checkbox" class="peer" aria-label="Ordinary peer">
    <span id="ordinary-peer-target" class="opacity-40 peer-checked:opacity-100">Ordinary peer target</span>
  </section>
  <section id="data-group" class="group" data-state="closed">
    <span id="data-target" class="text-slate-900 group-data-[state=open]:text-green-600">Data state</span>
  </section>
  <details id="open-group" class="group">
    <summary>Open state</summary>
    <span id="open-target" class="text-slate-900 group-open:text-green-600">Open target</span>
  </details>
  <section id="destructive-group" class="group">
    <button id="before-destructive">Before destructive</button>
    <button id="destructive-target" class="bg-white text-slate-900 group-[.destructive]:text-red-500 group-[.destructive]:hover:bg-red-500 focus-visible:ring-2 group-[.destructive]:focus-visible:ring-red-500">Destructive state</button>
  </section>
  <section id="responsive-group" class="group" style="padding:12px">
    <span id="responsive-target" class="opacity-40 lg:group-hover:opacity-100">Responsive target</span>
    <span id="light-target" class="text-slate-900 light:group-hover:text-green-600">Light target</span>
  </section>
  <div id="pointer-park" style="position:fixed;right:0;top:0;width:20px;height:20px">Park</div>
</main></body></html>`

const candidates = [
  'hover:text-green-600', 'focus-visible:ring-2',
  'group-hover:opacity-100', 'group-hover:translate-x-1', 'group-hover/panel:translate-x-2',
  'group-focus-within:opacity-100', 'peer-checked/picker:text-green-600',
  'peer-checked:opacity-100',
  'peer-focus-visible/picker:opacity-100', 'peer-disabled/picker:opacity-25',
  'group-data-[state=open]:text-green-600', 'group-open:text-green-600',
  'group-[.destructive]:text-red-500', 'group-[.destructive]:hover:bg-red-500',
  'group-[.destructive]:focus-visible:ring-red-500', 'lg:group-hover:opacity-100',
  'light:group-hover:text-green-600',
]

async function compileFixture() {
  const lockBytes = readFileSync(new URL('package-lock.json', frontendURL))
  const lock = JSON.parse(lockBytes.toString('utf8')) as {
    packages: Record<string, { version: string }>
  }
  const tailwindPackagePath = require.resolve('tailwindcss/package.json')
  const tailwindRequire = createRequire(tailwindPackagePath)
  const parserPackagePath = tailwindRequire.resolve('postcss-selector-parser/package.json')
  // Refuse an unexpected nested copy: the proposed lock repair is exactly this
  // root node, not a silently different parser used by the compiler.
  expect(parserPackagePath).toBe(fileURLToPath(new URL('node_modules/postcss-selector-parser/package.json', frontendURL)))
  const versions = {
    tailwindcss: (require(tailwindPackagePath) as { version: string }).version,
    postcss: (require('postcss/package.json') as { version: string }).version,
    parser: (tailwindRequire(parserPackagePath) as { version: string }).version,
  }
  expect(versions.tailwindcss).toBe(lock.packages['node_modules/tailwindcss'].version)
  expect(versions.postcss).toBe(lock.packages['node_modules/postcss'].version)
  expect(versions.parser).toBe(lock.packages['node_modules/postcss-selector-parser'].version)
  const result = await postcss([tailwindcss({
    ...projectConfig,
    content: [{ raw: markup, extension: 'html' }],
  })]).process('@tailwind base;\n@tailwind components;\n@tailwind utilities;', { from: undefined })
  const selectors: string[] = []
  result.root.walkRules(rule => { selectors.push(rule.selector) })
  const presence = Object.fromEntries(candidates.map(candidate => {
    const escaped = candidate.replace(/[^a-zA-Z0-9_-]/g, '\\$&')
    return [candidate, selectors.some(selector => selector.includes(`.${escaped}`))]
  }))
  return {
    css: result.css,
    metadata: {
      versions, node: process.version,
      lockSHA256: digest(lockBytes),
      configSHA256: digest(readFileSync(new URL('tailwind.config.cjs', frontendURL))),
      specSHA256: digest(readFileSync(fileURLToPath(import.meta.url))),
      contentSHA256: digest(markup), cssSHA256: digest(result.css),
      cssBytes: Buffer.byteLength(result.css), presence,
      warnings: result.warnings().map(warning => warning.text),
    },
  }
}

let compilation: ReturnType<typeof compileFixture> | undefined

async function attachCompilerEvidence(testInfo: TestInfo) {
  compilation ??= compileFixture()
  const compiled = await compilation
  await testInfo.attach('locked-tailwind-compilation.json', {
    body: Buffer.from(JSON.stringify(compiled.metadata, null, 2)), contentType: 'application/json',
  })
  await testInfo.attach('locked-tailwind-fixture.css', { body: Buffer.from(compiled.css), contentType: 'text/css' })
  return compiled
}

async function mountFixture(page: Page, testInfo: TestInfo) {
  const compiled = await attachCompilerEvidence(testInfo)
  await page.route('**/*', route => route.abort())
  await page.setContent(markup)
  await page.addStyleTag({ content: compiled.css })
  // Hard baseline: later failures must not merely be a missing stylesheet.
  await expect(page.locator('#ordinary-hover')).toHaveCSS('color', 'rgb(15, 23, 42)')
  await page.locator('#pointer-park').hover()
}

async function css(locator: Locator, property: string) {
  return locator.evaluate((element, name) => getComputedStyle(element).getPropertyValue(name), property)
}

async function translateX(locator: Locator) {
  return locator.evaluate(element => {
    const transform = getComputedStyle(element).transform
    return transform === 'none' ? 0 : new DOMMatrixReadOnly(transform).m41
  })
}

test('locked Tailwind ordinary hover and keyboard focus are working controls', async ({ page }, testInfo) => {
  await mountFixture(page, testInfo)
  const hover = page.locator('#ordinary-hover')
  await hover.hover()
  await expect(hover).toHaveCSS('color', 'rgb(22, 163, 74)')
  await page.locator('#pointer-park').hover()
  await expect(hover).toHaveCSS('color', 'rgb(15, 23, 42)')
  const focus = page.locator('#ordinary-focus')
  const restShadow = await css(focus, 'box-shadow')
  await hover.focus()
  await page.keyboard.press('Tab')
  await expect(focus).toBeFocused()
  expect(await css(focus, 'box-shadow')).toContain('rgb(239, 68, 68)')
  expect(await css(focus, 'box-shadow')).not.toBe(restShadow)
  await page.keyboard.press('Tab')
  await expect(page.locator('#after-ordinary')).toBeFocused()
  expect(await css(focus, 'box-shadow')).toBe(restShadow)
})

test('locked Tailwind group and named group hover affect only their own descendants', async ({ page }, testInfo) => {
  await mountFixture(page, testInfo)
  const target = page.locator('#group-target')
  const named = page.locator('#named-target')
  const other = page.locator('#group-control')
  expect(await css(target, 'opacity')).toBe('0.4')
  expect(await translateX(named)).toBe(0)
  await page.locator('#group-a').hover()
  expect.soft(await css(target, 'opacity')).toBe('1')
  expect.soft(await translateX(target)).toBe(4)
  expect.soft(await translateX(named)).toBe(8)
  expect(await css(other, 'opacity')).toBe('0.4')
  expect(await translateX(other)).toBe(0)
  await page.locator('#pointer-park').hover()
  expect(await css(target, 'opacity')).toBe('0.4')
  expect(await translateX(target)).toBe(0)
  expect(await translateX(named)).toBe(0)
})

test('locked Tailwind group focus-within responds to keyboard entry and exit', async ({ page }, testInfo) => {
  await mountFixture(page, testInfo)
  const target = page.locator('#within-target')
  expect(await css(target, 'opacity')).toBe('0.4')
  await page.locator('#before-within').focus()
  await page.keyboard.press('Tab')
  await expect(page.locator('#within-button')).toBeFocused()
  expect.soft(await css(target, 'opacity')).toBe('1')
  await page.keyboard.press('Tab')
  await expect(page.locator('#after-within')).toBeFocused()
  expect(await css(target, 'opacity')).toBe('0.4')
})

test('locked Tailwind named peer checked focus and disabled states remain isolated', async ({ page }, testInfo) => {
  await mountFixture(page, testInfo)
  const peer = page.locator('#peer-a')
  const checked = page.locator('#peer-checked')
  const focus = page.locator('#peer-focus')
  const disabled = page.locator('#peer-disabled')
  expect(await css(checked, 'color')).toBe('rgb(15, 23, 42)')
  expect(await css(focus, 'opacity')).toBe('0.4')
  await page.locator('#before-peer').focus()
  await page.keyboard.press('Tab')
  await expect(peer).toBeFocused()
  expect.soft(await css(focus, 'opacity')).toBe('1')
  await page.keyboard.press('Space')
  await expect(peer).toBeChecked()
  expect.soft(await css(checked, 'color')).toBe('rgb(22, 163, 74)')
  await expect(page.locator('#peer-b')).not.toBeChecked()
  expect(await css(page.locator('#peer-control'), 'color')).toBe('rgb(15, 23, 42)')
  await page.keyboard.press('Space')
  await expect(peer).not.toBeChecked()
  expect(await css(checked, 'color')).toBe('rgb(15, 23, 42)')
  await page.keyboard.press('Tab')
  expect(await css(focus, 'opacity')).toBe('0.4')
  await peer.evaluate((element: HTMLInputElement) => { element.disabled = true })
  await expect(peer).toBeDisabled()
  expect.soft(await css(disabled, 'opacity')).toBe('0.25')
  expect.soft(await css(disabled, 'cursor')).toBe('not-allowed')
  await peer.evaluate((element: HTMLInputElement) => { element.disabled = false })
  expect(await css(disabled, 'opacity')).toBe('1')
  expect(await css(disabled, 'cursor')).toBe('default')
  await page.locator('#ordinary-peer').check()
  await expect(page.locator('#ordinary-peer')).toBeChecked()
  expect.soft(await css(page.locator('#ordinary-peer-target'), 'opacity')).toBe('1')
  expect(await css(page.locator('#peer-control'), 'color')).toBe('rgb(15, 23, 42)')
  await page.locator('#ordinary-peer').uncheck()
  expect(await css(page.locator('#ordinary-peer-target'), 'opacity')).toBe('0.4')
})

test('locked Tailwind data open and destructive group states compile and respond', async ({ page }, testInfo) => {
  await mountFixture(page, testInfo)
  const data = page.locator('#data-target')
  expect(await css(data, 'color')).toBe('rgb(15, 23, 42)')
  await page.locator('#data-group').evaluate(element => element.setAttribute('data-state', 'open'))
  expect.soft(await css(data, 'color')).toBe('rgb(22, 163, 74)')
  await page.locator('#data-group').evaluate(element => element.setAttribute('data-state', 'closed'))
  expect(await css(data, 'color')).toBe('rgb(15, 23, 42)')
  await page.locator('#open-group summary').click()
  await expect(page.locator('#open-group')).toHaveAttribute('open', '')
  await expect(page.locator('#open-target')).toBeVisible()
  expect.soft(await css(page.locator('#open-target'), 'color')).toBe('rgb(22, 163, 74)')
  await page.locator('#open-group summary').click()
  expect(await css(page.locator('#open-target'), 'color')).toBe('rgb(15, 23, 42)')
  const destructive = page.locator('#destructive-target')
  await destructive.hover()
  expect(await css(destructive, 'background-color')).toBe('rgb(255, 255, 255)')
  await page.locator('#destructive-group').evaluate(element => element.classList.add('destructive'))
  expect.soft(await css(destructive, 'color')).toBe('rgb(239, 68, 68)')
  expect.soft(await css(destructive, 'background-color')).toBe('rgb(239, 68, 68)')
  await page.locator('#pointer-park').hover()
  expect(await css(destructive, 'background-color')).toBe('rgb(255, 255, 255)')
  await page.locator('#before-destructive').focus()
  await page.keyboard.press('Tab')
  await expect(destructive).toBeFocused()
  expect.soft(await css(destructive, 'box-shadow')).toContain('rgb(239, 68, 68)')
  await page.locator('#destructive-group').evaluate(element => element.classList.remove('destructive'))
  expect(await css(destructive, 'color')).toBe('rgb(15, 23, 42)')
  expect(await css(destructive, 'box-shadow')).not.toContain('rgb(239, 68, 68)')
})

test('locked Tailwind responsive and configured light group hover keep their conditions', async ({ page }, testInfo) => {
  await mountFixture(page, testInfo)
  const group = page.locator('#responsive-group')
  const target = page.locator('#responsive-target')
  const light = page.locator('#light-target')
  await page.setViewportSize({ width: 1000, height: 1000 })
  await group.hover()
  expect(await css(target, 'opacity')).toBe('0.4')
  expect(await css(light, 'color')).toBe('rgb(15, 23, 42)')
  await page.setViewportSize({ width: 1100, height: 1000 })
  await group.hover()
  expect.soft(await css(target, 'opacity')).toBe('1')
  await page.locator('html').evaluate(element => element.classList.add('light'))
  expect.soft(await css(light, 'color')).toBe('rgb(22, 163, 74)')
  expect(await css(page.locator('#group-control'), 'opacity')).toBe('0.4')
  await page.locator('#pointer-park').hover()
  expect(await css(target, 'opacity')).toBe('0.4')
  expect(await css(light, 'color')).toBe('rgb(15, 23, 42)')
  await page.locator('html').evaluate(element => element.classList.remove('light'))
  await page.setViewportSize({ width: 1000, height: 1000 })
  await group.hover()
  expect(await css(target, 'opacity')).toBe('0.4')
})

test('built public About arrow translates on parent hover and restores without changing keyboard navigation', async ({ page, baseURL }, testInfo) => {
  await attachCompilerEvidence(testInfo)
  const origin = new URL(baseURL ?? '').origin
  expect(['localhost', '127.0.0.1', '[::1]']).toContain(new URL(origin).hostname)
  await page.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort())
  await page.goto('/about')
  await expect(page.getByRole('heading', { name: /Every signal\. One reply\./ })).toBeVisible()
  const assets = await page.locator('script[type="module"][src], link[rel="stylesheet"][href]').evaluateAll(elements => elements.map(element =>
    element instanceof HTMLScriptElement ? element.src : (element as HTMLLinkElement).href,
  ))
  expect(assets.some(url => new URL(url).pathname.startsWith('/assets/') && new URL(url).pathname.endsWith('.js')),
    'this contract must exercise the built app, not Vite source/dev serving').toBe(true)
  expect(assets.every(url => !new URL(url).pathname.includes('/@vite/'))).toBe(true)
  const link = page.getByRole('link', { name: 'Sign in', exact: true })
  const arrow = link.locator('svg')
  await expect(arrow).toHaveCount(1)
  await page.mouse.move(0, 0)
  const rest = await translateX(arrow)
  expect(rest).toBe(0)
  const expectedX = await page.evaluate(() => parseFloat(getComputedStyle(document.documentElement).fontSize) * 0.125)
  let hovered = rest
  let restored = rest
  try {
    await link.hover()
    // Soft failure retains the independent reset/focus observations and CSS
    // diagnostics on the original lock. This remains a FAILED regression.
    await expect.configure({ soft: true }).poll(async () => {
      hovered = await translateX(arrow)
      return hovered
    }).toBeCloseTo(expectedX, 2)
    await page.mouse.move(0, 0)
    await expect.poll(async () => {
      restored = await translateX(arrow)
      return restored
    }).toBe(0)
    await page.evaluate(() => { (document.activeElement as HTMLElement | null)?.blur() })
    for (let step = 0; step < 16 && !(await link.evaluate(element => document.activeElement === element)); step++) {
      await page.keyboard.press('Tab')
    }
    await expect(link).toBeFocused()
    await expect(link).toHaveAttribute('href', '/login')
    // The product declares a hover translation, not a focus translation. No
    // external navigation and no invented keyboard-animation assertion.
  } finally {
    await testInfo.attach('built-about-arrow.json', {
      body: Buffer.from(JSON.stringify({ assets, rest, hovered, restored, expectedX }, null, 2)),
      contentType: 'application/json',
    })
  }
})
