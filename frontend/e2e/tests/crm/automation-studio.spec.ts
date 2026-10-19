import { test, expect, type Page } from '@playwright/test'
import { loginAsAdmin } from '../../helpers'
import { AutomationStudioPage } from '../../pages'

async function enableCRMEntitlement(page: Page) {
  await page.route('**/api/product/entitlements**', async (route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        data: {
          mode: 'licensed',
          entitlements: {
            'crm.enabled': true,
          },
        },
      }),
    })
  })
}

type MeasurementStatus = {
  queued: number
  heldNodes: number
  forwardedNodes: number
  elapsed: number
  overflow: boolean
}
type MeasurementGate = {
  status: () => MeasurementStatus
  release: () => void
  restore: () => void
  holdPastInitialTimer: () => Promise<void>
}

async function holdRealNodeMeasurements(page: Page) {
  await page.evaluate(() => {
    const NativeObserver = window.ResizeObserver
    const host = window as unknown as { __codexNodeMeasurements?: MeasurementGate }
    if (host.__codexNodeMeasurements) throw new Error('Measurement gate already installed')
    let holding = true
    let firstHeldAt: number | null = null
    let forwardedNodes = 0
    let overflow = false
    const heldTargets = new Set<Element>()
    const observers = new Set<DelayedNodeObserver>()
    const isNode = (target: Element) => target.matches('.vue-flow__node') &&
      Boolean(target.closest('[data-testid="automation-canvas"]'))

    // Deliver real native entries; never replace dimensions or VueFlow callbacks.
    class DelayedNodeObserver implements ResizeObserver {
      readonly native: ResizeObserver
      readonly targets = new Set<Element>()
      readonly pending: ResizeObserverEntry[][] = []
      constructor(readonly callback: ResizeObserverCallback) {
        observers.add(this)
        this.native = new NativeObserver((entries) => {
          const immediate: ResizeObserverEntry[] = []
          const delayed: ResizeObserverEntry[] = []
          for (const entry of entries) {
            if (!this.targets.has(entry.target)) continue
            if (holding && isNode(entry.target)) {
              firstHeldAt ??= performance.now()
              heldTargets.add(entry.target)
              delayed.push(entry)
            } else {
              if (isNode(entry.target)) forwardedNodes++
              immediate.push(entry)
            }
          }
          if (delayed.length) {
            const queued = [...observers].reduce((n, observer) => n + observer.pending.flat().length, 0)
            if (queued + delayed.length > 256) overflow = true
            else this.pending.push(delayed)
          }
          if (immediate.length) this.callback(immediate, this)
        })
      }
      observe(target: Element, options?: ResizeObserverOptions) {
        this.targets.add(target)
        this.native.observe(target, options)
      }
      unobserve(target: Element) {
        this.targets.delete(target)
        for (let i = this.pending.length - 1; i >= 0; i--) {
          this.pending[i] = this.pending[i].filter(entry => entry.target !== target)
          if (!this.pending[i].length) this.pending.splice(i, 1)
        }
        this.native.unobserve(target)
      }
      disconnect() {
        this.targets.clear()
        this.pending.length = 0
        this.native.disconnect()
      }
      release() {
        const batches = this.pending.splice(0)
        for (const batch of batches) {
          const live = batch.filter(entry => this.targets.has(entry.target) && entry.target.isConnected)
          forwardedNodes += live.filter(entry => isNode(entry.target)).length
          if (live.length) this.callback(live, this)
        }
      }
    }
    const release = () => {
      holding = false
      for (const observer of observers) observer.release()
    }
    window.ResizeObserver = DelayedNodeObserver
    host.__codexNodeMeasurements = {
      status: () => ({
        queued: [...observers].reduce((n, observer) => n + observer.pending.flat().length, 0),
        heldNodes: heldTargets.size, forwardedNodes,
        elapsed: firstHeldAt === null ? 0 : performance.now() - firstHeldAt, overflow,
      }),
      release,
      restore: () => {
        try { release() } finally {
          window.ResizeObserver = NativeObserver
          delete host.__codexNodeMeasurements
        }
      },
      holdPastInitialTimer: async () => {
        if (firstHeldAt === null) throw new Error('No real node measurement was held')
        // Perturbation only: native timers continue, including the existing 80ms fit.
        // The pass/fail oracle below is real geometry and ordinary UI interaction.
        await new Promise(resolve => window.setTimeout(resolve, Math.max(0, 200 - (performance.now() - firstHeldAt!))))
      },
    }
  })
  const gate = () => page.evaluate(() => {
    const value = (window as unknown as { __codexNodeMeasurements?: MeasurementGate }).__codexNodeMeasurements
    if (!value) throw new Error('Measurement gate missing')
    return value.status()
  })
  return {
    status: gate,
    hold: () => page.evaluate(() => (window as unknown as { __codexNodeMeasurements: MeasurementGate }).__codexNodeMeasurements.holdPastInitialTimer()),
    release: () => page.evaluate(() => (window as unknown as { __codexNodeMeasurements: MeasurementGate }).__codexNodeMeasurements.release()),
    restore: () => page.evaluate(() => (window as unknown as { __codexNodeMeasurements: MeasurementGate }).__codexNodeMeasurements.restore()),
  }
}

async function graphGeometry(page: Page) {
  return page.getByTestId('automation-canvas').evaluate(canvas => {
    const bounds = canvas.getBoundingClientRect()
    const pane = canvas.querySelector('.vue-flow__transformationpane')
    const transform = new DOMMatrixReadOnly(pane ? getComputedStyle(pane).transform : undefined)
    const nodes = [...canvas.querySelectorAll('.vue-flow__node')].map(node => {
      const rect = node.getBoundingClientRect()
      return {
        label: node.getAttribute('aria-label'),
        measured: rect.width > 0 && rect.height > 0,
        contained: rect.width > 0 && rect.height > 0 &&
          rect.x >= Math.max(0, bounds.x) - 1 && rect.y >= Math.max(0, bounds.y) - 1 &&
          rect.right <= Math.min(innerWidth, bounds.right) + 1 && rect.bottom <= Math.min(innerHeight, bounds.bottom) + 1,
      }
    })
    return { nodes, transform: { x: transform.e, y: transform.f, zoom: transform.a } }
  })

test.describe('CRM Automation Studio', () => {
  let studio: AutomationStudioPage

  test.beforeEach(async ({ page }) => {
    await enableCRMEntitlement(page)
    await loginAsAdmin(page)
    studio = new AutomationStudioPage(page)
    await studio.gotoList()
  })

  test('offers the four task-first starter policies', async ({ page }) => {
    await expect(studio.policyList).toBeVisible()
    for (const key of ['post-visit', 'no-show', 'package-care', 'overdue-invoice']) {
      await expect(page.getByTestId(`automation-template-${key}`)).toBeVisible()
    }
    await expect(page.getByText('Customer messages stay locked')).toBeVisible()
  })

  test('opens the post-visit visual graph with a meaningful safe preview', async ({ page }) => {
    await studio.useTemplate('post-visit')

    await expect(studio.palette).toBeVisible()
    await expect(studio.canvas).toBeVisible()
    await expect(page.getByLabel('Policy name')).toHaveValue('Post-visit care')
    await expect(page.getByText('booking.status_changed')).toBeVisible()
    await studio.canvas.getByText('Create follow-up', { exact: true }).click()
    await expect(page.getByText('Internal action only')).toBeVisible()
    await expect(studio.saveDraft).toBeEnabled()
    await expect(studio.activate).toBeVisible()
  })

  test('package care listens for both low balance and expiry events', async ({ page }) => {
    await studio.useTemplate('package-care')

    await expect(page.getByText(/package\.balance_low or package\.expiring/)).toBeVisible()
    await expect(page.getByRole('button', { name: /Send message/ })).toBeDisabled()
  })

  test('keeps the builder usable on a narrow viewport', async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 })
    await studio.useTemplate('no-show')

    await expect(studio.canvas).toBeVisible()
    await expect(studio.palette).toBeVisible()
    await expect(page.getByRole('button', { name: 'Wait' })).toBeVisible()
  })

  for (const mobile of [false, true]) {
    test(mobile
      ? 'frames the first mobile step after delayed real node measurements'
      : 'automatically fits delayed real node measurements and preserves later user panning', async ({ page }, testInfo) => {
      if (mobile) await page.setViewportSize({ width: 390, height: 844 })
      const gate = await holdRealNodeMeasurements(page)
      let held: MeasurementStatus | null = null
      try {
        await studio.useTemplate('post-visit')
        await expect(studio.canvas).toBeVisible()
        await expect.poll(async () => (await gate.status()).heldNodes).toBe(4)
        await gate.hold()
        held = await gate.status()
        expect(held.queued).toBeGreaterThanOrEqual(4)
        expect(held.elapsed).toBeGreaterThanOrEqual(190)
        expect(held.overflow).toBe(false)
        await gate.release()
        await expect.poll(async () => (await graphGeometry(page)).nodes.filter(node => node.measured).length).toBe(4)

        if (mobile) {
          await expect.poll(async () => {
            const graph = await graphGeometry(page)
            return graph.nodes.find(node => node.label === 'trigger automation step')?.contained === true &&
              graph.transform.zoom >= 0.6 - 0.001 && graph.transform.zoom <= 0.82 + 0.001
          }).toBe(true)
          await studio.canvas.locator('.vue-flow__node[aria-label="trigger automation step"]').click()
          await expect(page.getByRole('dialog', { name: 'Step settings' })).toBeVisible()
        } else {
          await expect.poll(async () => (await graphGeometry(page)).nodes.every(node => node.contained)).toBe(true)
          await studio.canvas.getByText('Create follow-up', { exact: true }).click()
          await expect(page.getByText('Internal action only')).toBeVisible()
          await expect(studio.saveDraft).toBeEnabled()
          await expect(studio.activate).toBeVisible()

          const beforePan = (await graphGeometry(page)).transform
          const box = await studio.canvas.locator('.vue-flow__pane').boundingBox()
          if (!box) throw new Error('Canvas pane missing')
          await page.mouse.move(box.x + box.width * 0.5, box.y + box.height * 0.85)
          await page.mouse.down()
          await page.mouse.move(box.x + box.width * 0.5 + 40, box.y + box.height * 0.85 + 20, { steps: 5 })
          await page.mouse.up()
          await expect.poll(async () => (await graphGeometry(page)).transform).not.toEqual(beforePan)
          const panned = (await graphGeometry(page)).transform
          const beforeNewNode = (await gate.status()).forwardedNodes
          await studio.palette.getByRole('button', { name: /^Wait(?:\s|$)/ }).click()
          await expect.poll(async () => (await graphGeometry(page)).nodes.filter(node => node.measured).length).toBe(5)
          await expect.poll(async () => (await gate.status()).forwardedNodes).toBeGreaterThan(beforeNewNode)
          await page.evaluate(() => new Promise<void>(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))))
          expect((await graphGeometry(page)).transform).toEqual(panned)
        }
        expect((await gate.status()).overflow).toBe(false)
      } finally {
        const final = await gate.status()
        await gate.restore()
        await testInfo.attach('real-node-measurement-delay', {
          body: Buffer.from(JSON.stringify({ mobile, held, final, nativeEntriesOnly: true })),
          contentType: 'application/json',
        })
      }
    })
  }
})
