import { expect, test, type Page } from '@playwright/test'

const organization = {
  id: '71111111-1111-4111-8111-111111111111',
  name: 'ReAlign Kajang',
  slug: 'realign-kajang',
}

const platformOwner = {
  id: '75555555-5555-4555-8555-555555555555',
  email: 'owner@example.test',
  full_name: 'Platform Owner',
  organization_id: organization.id,
  organization_name: organization.name,
  is_super_admin: true,
  is_reseller_admin: false,
}

const threadsAccount = {
  id: '72222222-2222-4222-8222-222222222222',
  channel: 'threads',
  provider: 'threads',
  name: 'ReAlign Kajang Threads',
  external_account_id: 'realign-threads',
  status: 'active',
  capabilities: {
    text: true,
    replies: true,
    public_replies: true,
    reply_target_required: true,
    mentions: true,
    business_initiation: false,
    direct_messages: false,
  },
  config: {
    outbound_enabled: true,
    engagement_mode: 'public_replies_mentions',
    direct_messages_supported: false,
  },
  has_credentials: true,
  outbox_pending: 0,
  outbox_failed: 0,
}

const threadsConversation = {
  id: '73333333-3333-4333-8333-333333333333',
  channel_account_id: threadsAccount.id,
  contact_id: '74444444-4444-4444-8444-444444444444',
  channel: 'threads',
  external_conversation_id: 'threads-public-mention-987',
  subject: 'Mentioned ReAlign',
  status: 'open',
  last_message_preview: '@realign Can Pilates help my back?',
  last_message_at: '2026-07-28T08:00:00Z',
  unread_count: 0,
  metadata: { engagement_type: 'mention' },
  contact: {
    id: '74444444-4444-4444-8444-444444444444',
    profile_name: 'Alya Public',
  },
}

async function mockChannels(
  page: Page,
  options: {
    threadsEnabled: boolean
    accounts?: typeof threadsAccount[]
    conversations?: typeof threadsConversation[]
  },
) {
  let createdAccount: Record<string, unknown> | null = null
  let sentMessage: Record<string, unknown> | null = null

  await page.addInitScript(user => {
    window.localStorage.setItem('user', JSON.stringify(user))
  }, platformOwner)

  await page.route(/\/api\/me(?:\?.*)?$/, route =>
    route.fulfill({ json: { data: platformOwner } }),
  )
  await page.route(/\/api\/me\/organizations(?:\?.*)?$/, route =>
    route.fulfill({
      json: {
        data: {
          organizations: [{
            organization_id: organization.id,
            name: organization.name,
            slug: organization.slug,
            role_name: 'Platform Owner',
            is_default: true,
          }],
        },
      },
    }),
  )
  await page.route(/\/api\/organizations(?:\?.*)?$/, route =>
    route.fulfill({ json: { data: { organizations: [organization] } } }),
  )
  await page.route(/\/api\/product\/entitlements(?:\?.*)?$/, route =>
    route.fulfill({
      json: {
        data: {
          plan_code: 'omnitech-growth',
          mode: 'subscription',
          entitlements: {
            'omnichannel.enabled': true,
            ...(options.threadsEnabled
              ? { 'threads.public_engagement.enabled': true }
              : {}),
          },
          overridden_keys: [],
          evaluated_at: '2026-07-28T08:00:00Z',
        },
      },
    }),
  )
  await page.route(/\/api\/auth\/ws-token(?:\?.*)?$/, route =>
    route.fulfill({ json: { data: { token: '' } } }),
  )
  await page.route(/\/api\/channel-accounts(?:\?.*)?$/, async route => {
    if (route.request().method() === 'POST') {
      createdAccount = route.request().postDataJSON() as Record<string, unknown>
      await route.fulfill({
        json: {
          data: {
            account: {
              ...threadsAccount,
              status: 'pending',
              capabilities: {
                public_replies: false,
                mentions: false,
                business_initiation: false,
                direct_messages: false,
              },
              config: {
                ...threadsAccount.config,
                outbound_enabled: false,
                beta: true,
                activation_available: false,
              },
            },
            inbound_secret: 'one-time-signing-secret',
            webhook_path: `/api/webhooks/channels/${threadsAccount.id}`,
          },
        },
      })
      return
    }
    await route.fulfill({
      json: { data: { accounts: options.accounts ?? [] } },
    })
  })
  await page.route(/\/api\/conversations(?:\?.*)?$/, route =>
    route.fulfill({
      json: {
        data: {
          conversations: options.conversations ?? [],
          total: options.conversations?.length ?? 0,
        },
      },
    }),
  )
  await page.route(
    new RegExp(`/api/contacts/${threadsConversation.contact_id}/workspace(?:\\?.*)?$`),
    route =>
      route.fulfill({
        json: {
          data: {
            contact: threadsConversation.contact,
            capabilities: {
              crm: false,
              tasks: false,
              bookings: false,
              packages: false,
              payments: false,
              copilot: false,
              merge: false,
            },
            identities: [],
            journeys: [],
            tasks: [],
            bookings: [],
            packages: [],
            invoices: [],
            payments: [],
            summary: {
              pipeline_value: [],
              outstanding: [],
              collected: [],
            },
            timeline: [],
          },
        },
      }),
  )
  await page.route(
    new RegExp(`/api/conversations/${threadsConversation.id}/messages(?:\\?.*)?$`),
    async route => {
      if (route.request().method() === 'POST') {
        sentMessage = route.request().postDataJSON() as Record<string, unknown>
        await route.fulfill({
          json: {
            data: {
              message: {
                id: '76666666-6666-4666-8666-666666666666',
                direction: 'outgoing',
                message_type: 'text',
                content: 'Yes, Pilates can help.',
                status: 'pending',
                created_at: '2026-07-28T08:01:00Z',
              },
              parts: [{ type: 'text', text: 'Yes, Pilates can help.' }],
            },
          },
        })
        return
      }
      await route.fulfill({ json: { data: { messages: [], total: 0 } } })
    },
  )

  return {
    createdAccount: () => createdAccount,
    sentMessage: () => sentMessage,
  }
}

test.describe('Threads public engagement channel', () => {
  test('stays hidden when the workspace lacks the Threads entitlement', async ({ page }) => {
    await mockChannels(page, { threadsEnabled: false })
    await page.goto('/inbox')

    await page.getByRole('button', { name: 'Connect', exact: true }).click()
    await expect(
      page.getByTestId('channel-connect-type').locator('option[value="threads"]'),
    ).toHaveCount(0)
    await expect(page.getByTestId('threads-public-engagement-adapter')).toHaveCount(0)
  })

  test('routes account authorization through Integrations instead of generic channel creation', async ({ page }) => {
    const state = await mockChannels(page, { threadsEnabled: true })
    await page.goto('/inbox')

    await page.getByRole('button', { name: 'Connect', exact: true }).click()
    const channelSelect = page.getByTestId('channel-connect-type')
    await expect(channelSelect.locator('option[value="threads"]')).toHaveCount(0)
    await expect(page.getByTestId('threads-public-engagement-adapter')).toContainText(
      'OAuth managed in Settings → Integrations',
    )
    expect(state.createdAccount()).toBeNull()
  })

  test('sends a public reply to the exact selected Threads mention target', async ({ page }) => {
    const state = await mockChannels(page, {
      threadsEnabled: true,
      accounts: [threadsAccount],
      conversations: [threadsConversation],
    })
    await page.goto('/inbox')

    await expect(page.getByTestId('threads-manage-in-integrations').first()).toHaveAttribute(
      'href',
      '/settings/integrations',
    )
    await page.getByRole('button', { name: /Alya Public/ }).click()
    const notice = page.getByTestId('threads-public-reply-composer-notice')
    await expect(notice).toContainText('selected public Threads mention')
    await expect(notice).toContainText(
      'Direct messages and standalone posts are not supported',
    )

    const composer = page.getByPlaceholder('Write a public Threads reply...')
    await expect(composer).toBeEnabled()
    await composer.fill('Yes, Pilates can help.')
    await page.getByRole('button', { name: 'Send public Threads reply' }).click()

    await expect.poll(state.sentMessage).toMatchObject({
      purpose: 'service',
      reply_to_external_id: 'threads-public-mention-987',
      parts: [{ type: 'text', text: 'Yes, Pilates can help.' }],
    })
  })

  test('keeps direct-message-shaped Threads conversations fail-closed', async ({ page }) => {
    const state = await mockChannels(page, {
      threadsEnabled: true,
      accounts: [threadsAccount],
      conversations: [
        {
          ...threadsConversation,
          metadata: { engagement_type: 'direct_message' },
        },
      ],
    })
    await page.goto('/inbox')

    await page.getByRole('button', { name: /Alya Public/ }).click()
    const notice = page.getByTestId('threads-public-reply-composer-notice')
    await expect(notice).toContainText(
      'Select an existing public Threads reply or mention before replying',
    )
    await expect(notice).toContainText(
      'Direct messages and standalone posts are not supported',
    )
    await expect(
      page.getByPlaceholder('Select an existing public Threads reply or mention'),
    ).toBeDisabled()
    await expect(
      page.getByRole('button', { name: 'Threads reply unavailable' }),
    ).toBeDisabled()
    expect(state.sentMessage()).toBeNull()
  })
})
