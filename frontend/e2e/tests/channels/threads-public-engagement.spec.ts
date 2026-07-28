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
  provider: 'relay',
  name: 'ReAlign Threads beta',
  external_account_id: 'realign-threads',
  status: 'active',
  capabilities: {
    text: true,
    replies: true,
    business_initiation: false,
    direct_messages: false,
  },
  config: {
    relay_url: 'https://relay.example.test/threads',
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

  test('shows beta public-only guidance and creates a relay account when entitled', async ({ page }) => {
    const state = await mockChannels(page, { threadsEnabled: true })
    await page.goto('/inbox')

    await page.getByRole('button', { name: 'Connect', exact: true }).click()
    const channelSelect = page.getByTestId('channel-connect-type')
    await expect(channelSelect.locator('option[value="threads"]')).toHaveText(
      'Threads public replies',
    )
    await channelSelect.selectOption('threads')

    const notice = page.getByTestId('threads-public-engagement-notice')
    await expect(notice).toContainText('Beta: public engagement only')
    await expect(notice).toContainText('Direct messages and standalone posts are not supported')
    await expect(notice).toContainText('remains pending')

    await page.getByPlaceholder('Connection name').fill('ReAlign Threads beta')
    await page.getByPlaceholder('External account ID').fill('realign-threads')
    await page.getByPlaceholder('HTTPS signed-relay URL').fill(
      'https://relay.example.test/threads',
    )
    await page.getByTestId('channel-connect-submit').click()

    await expect.poll(state.createdAccount).toMatchObject({
      channel: 'threads',
      provider: 'relay',
      name: 'ReAlign Threads beta',
      external_account_id: 'realign-threads',
      config: { relay_url: 'https://relay.example.test/threads' },
    })
    expect(state.createdAccount()).not.toHaveProperty('is_default_outgoing', true)
  })

  test('sends only against the selected public reply or mention target', async ({ page }) => {
    const state = await mockChannels(page, {
      threadsEnabled: true,
      accounts: [threadsAccount],
      conversations: [threadsConversation],
    })
    await page.goto('/inbox')

    await page.getByRole('button', { name: /Alya Public/ }).click()
    const notice = page.getByTestId('threads-public-reply-composer-notice')
    await expect(notice).toContainText('selected reply or mention is the required target')
    await expect(notice).toContainText('direct messages and standalone posts are not supported')

    await page.getByPlaceholder('Write a public Threads reply...').fill(
      'Yes, Pilates can help.',
    )
    await page.getByRole('button', { name: 'Send public Threads reply', exact: true }).click()

    await expect.poll(state.sentMessage).toMatchObject({
      purpose: 'service',
      reply_to_external_id: threadsConversation.external_conversation_id,
      parts: [{ type: 'text', text: 'Yes, Pilates can help.' }],
    })
  })
})
