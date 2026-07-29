import { type Locator, type Page } from '@playwright/test'
import { BasePage } from './BasePage'

export class AutomationStudioPage extends BasePage {
  readonly policyList: Locator
  readonly palette: Locator
  readonly canvas: Locator
  readonly saveDraft: Locator
  readonly preview: Locator
  readonly activate: Locator

  constructor(page: Page) {
    super(page)
    this.policyList = page.getByTestId('automation-policy-list')
    this.palette = page.getByTestId('automation-palette')
    this.canvas = page.getByTestId('automation-canvas')
    this.saveDraft = page.getByTestId('automation-save-draft')
    this.preview = page.getByTestId('automation-preview')
    this.activate = page.getByTestId('automation-activate')
  }

  async gotoList() {
    await this.page.goto('/crm/automations', { waitUntil: 'domcontentloaded' })
  }

  async useTemplate(key: 'post-visit' | 'no-show' | 'package-care' | 'overdue-invoice') {
    await this.page.getByTestId(`automation-template-${key}`).click()
    await this.canvas.waitFor()
  }
}
