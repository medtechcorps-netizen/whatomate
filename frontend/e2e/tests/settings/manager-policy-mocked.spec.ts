import { expect, test, type Page } from '@playwright/test'

const organizationId = '21111111-1111-4111-8111-111111111111'

const permission = (resource: string, action = 'read') => ({
  id: `${resource}-${action}`,
  resource,
  action,
})

const managerUser = {
  id: '22222222-2222-4222-8222-222222222222',
  email: 'manager-policy@example.test',
  full_name: 'Workspace Manager',
  organization_id: organizationId,
  organization_name: 'Klinik Relive',
  is_super_admin: false,
  is_reseller_admin: false,
  settings: {
    email_notifications: true,
    new_message_alerts: true,
    campaign_updates: false,
  },
  role: {
    id: '23333333-3333-4333-8333-333333333333',
    name: 'manager',
    description: 'System manager',
    is_system: true,
    permissions: [
      permission('settings.general'),
      permission('settings.chatbot'),
      permission('settings.chatbot', 'write'),
      permission('chatbot.ai'),
      permission('chatbot.ai', 'write'),
      permission('accounts'),
      permission('accounts', 'write'),
      permission('contacts'),
      permission('canned_responses'),
      permission('tags'),
      permission('teams'),
    ],
  },
}

async function installManagerPolicyMocks(page: Page) {
  await page.addInitScript((user) => {
    window.localStorage.setItem('user', JSON.stringify(user))
    window.localStorage.setItem('locale', 'en')
  }, managerUser)

  // Register the catch-all first; Playwright gives the later specific routes
  // priority and this keeps unrelated shell requests deterministic.
  await page.route('**/api/**', (route) =>
    route.fulfill({ json: { data: {} } }),
  )
  await page.route(/\/api\/me(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: managerUser } }),
  )
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          mode: 'licensed',
          plan_code: 'rereply-growth',
          entitlements: {},
        },
      },
    }),
  )
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, (route) =>
    route.fulfill({ json: { data: { token: '' } } }),
  )
  await page.route(/\/api\/org\/settings(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          name: 'Klinik Relive',
          settings: {
            timezone: 'Asia/Kuala_Lumpur',
            date_format: 'DD/MM/YYYY',
            mask_phone_numbers: true,
          },
        },
      },
    }),
  )
  await page.route(/\/api\/users(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          users: [],
          total: 0,
          page: 1,
          limit: 50,
          online_count: 0,
        },
      },
    }),
  )
  await page.route(/\/api\/chatbot\/settings(?:\?.*)?$/, (route) =>
    route.fulfill({
      json: {
        data: {
          settings: {
            enabled: true,
            greeting_message: 'Hello',
            greeting_buttons: [],
            fallback_message: 'A team member will help shortly.',
            fallback_buttons: [],
            session_timeout_minutes: 30,
            business_hours_enabled: false,
            business_hours: [],
            allow_automated_outside_hours: true,
            allow_agent_queue_pickup: true,
            assign_to_same_agent: true,
            agent_current_conversation_only: false,
            ai_enabled: true,
            ai_max_tokens: 700,
            ai_system_prompt: 'Answer using approved clinic information.',
            sla_enabled: false,
            sla_escalation_notify_ids: [],
          },
          stats: {
            total_sessions: 0,
            active_sessions: 0,
            messages_handled: 0,
            ai_responses: 0,
            agent_transfers: 0,
            keywords_count: 0,
            flows_count: 0,
            ai_contexts_count: 0,
          },
        },
      },
    }),
  )
}

test('manager policy stays minimal, operational, and provider-neutral', async ({ page }) => {
  await installManagerPolicyMocks(page)
  await page.goto('/settings')

  await expect(page.getByRole('tab', { name: 'General', exact: true })).toBeVisible()
  await expect(page.getByRole('tab', { name: 'Notifications', exact: true })).toBeVisible()
  await expect(page.getByRole('tab', { name: 'Calling', exact: true })).toHaveCount(0)
  await expect(page.locator('#org_name')).not.toBeEditable()
  await expect(page.getByText('Workspace details are read-only for your role.')).toBeVisible()
  await expect(page.getByText('Meta App Credentials', { exact: true })).toHaveCount(0)

  const sidebar = page.locator('aside')
  for (const item of ['Accounts', 'Contacts', 'Canned Responses', 'Tags', 'Teams']) {
    await expect(sidebar.getByRole('menuitem', { name: item, exact: true })).toHaveCount(1)
  }
  for (const item of ['Webhooks', 'Custom Actions', 'Privacy', 'Support']) {
    await expect(sidebar.getByRole('menuitem', { name: item, exact: true })).toHaveCount(0)
  }

  await page.goto('/settings/chatbot')
  await page.getByRole('tab', { name: 'AI', exact: true }).click()

  await expect(page.getByText('ReReply AI', { exact: true })).toBeVisible()
  await expect(page.locator('body')).not.toContainText(/Qwen|Alibaba/i)
  await expect(page.getByText('AI Provider', { exact: true })).toHaveCount(0)
  await expect(page.getByText('Model', { exact: true })).toHaveCount(0)
  await expect(page.getByText('API Key', { exact: true })).toHaveCount(0)
  await expect(page.locator('input[type="password"]')).toHaveCount(0)
})
