import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'
import { request } from '@playwright/test'
import { Client } from 'pg'
import { ProductAPI } from '../canary/provision.ts'
import { SUPER_ADMIN } from '../framework/auth.ts'

async function preseed() {
  const base = new URL(process.env.BASE_URL || 'invalid:')
  const database = new URL(process.env.TEST_DATABASE_URL || 'invalid:')
  if (base.protocol !== 'http:' || base.hostname !== '127.0.0.1' || !base.port ||
      base.pathname !== '/' || base.search || base.hash || base.username || base.password ||
      !['postgres:', 'postgresql:'].includes(database.protocol) || database.hostname !== '127.0.0.1' ||
      !database.port || database.pathname !== '/whatomate_test' || database.hash ||
      !['', '?sslmode=disable'].includes(database.search) ||
      process.env.BOOKING_QUALIFICATION_WORKSPACE_ID !== undefined ||
      process.env.BOOKING_QUALIFICATION_WORKSPACE_NAME !== undefined ||
      process.env.BOOKING_QUALIFICATION_SCHEDULER !== undefined) {
    throw new Error('Booking fixture requires a fresh, owned loopback E2E app and database')
  }

  const context = await request.newContext({ baseURL: base.origin, timeout: 15_000 })
  const db = new Client({ connectionString: database.href, connectionTimeoutMillis: 5_000, statement_timeout: 5_000 })
  try {
    // Join the ordinary E2E migration's synthetic administrator to the
    // same application identity before provisioning anything.
    const api = new ProductAPI(context, undefined)
    await api.call('POST', '/api/auth/login', SUPER_ADMIN)
    const me = await api.call('GET', '/api/me')
    if (me.is_super_admin !== true || me.email !== SUPER_ADMIN.email || !me.id || !me.organization_id) {
      throw new Error('Synthetic E2E administrator required')
    }

    await db.connect()
    const actor = await db.query(
      `SELECT id FROM users
       WHERE id = $1 AND organization_id = $2 AND email = $3
         AND is_active AND is_super_admin AND deleted_at IS NULL`,
      [me.id, me.organization_id, SUPER_ADMIN.email],
    )
    if (actor.rowCount !== 1) throw new Error('E2E app and database identity differ')

    api.organizationID = me.organization_id
    const nonce = randomBytes(12).toString('hex')
    const planCode = 'e2e-booking-people-' + nonce
    const priceCode = planCode + '-monthly'
    const plan = await api.call('POST', '/api/admin/product/plans', {
      code: planCode,
      name: 'E2E-booking-people-' + nonce,
      status: 'active',
      is_public: false,
      prices: [{ code: priceCode, currency: 'MYR', unit_amount_minor: 0, interval: 'month' }],
      entitlements: [{ key: 'bookings.enabled', value_type: 'boolean', value: true }],
    })
    const createWorkspace = async (name: string) => {
      const workspace = await api.call('POST', '/api/organizations', { name })
      if (workspace.name !== name ||
          !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(workspace.id)) {
        throw new Error('The API did not create the intended synthetic workspace')
      }
      const subscription = await api.call('PUT', '/api/admin/organizations/' + workspace.id + '/subscription', {
        plan_id: plan.id,
        price_code: priceCode,
        status: 'active',
        manual_reference: planCode,
      })
      if (subscription.status !== 'active' || subscription.provider !== 'manual' || subscription.plan_id !== plan.id || !subscription.id) {
        throw new Error('The intended synthetic manual subscription is unavailable')
      }

      const saved = await db.query(
        `SELECT o.id FROM organizations o
           JOIN subscriptions s ON s.organization_id = o.id
          WHERE o.id = $1 AND o.name = $2 AND o.deleted_at IS NULL
            AND s.id = $3 AND s.plan_id = $4 AND s.provider = 'manual'
            AND s.status = 'active' AND s.deleted_at IS NULL
            AND s.current_period_end > CURRENT_TIMESTAMP
            AND s.entitlements_snapshot @> '{"bookings.enabled":true}'::jsonb`,
        [workspace.id, name, subscription.id, plan.id],
      )
      if (saved.rowCount !== 1) throw new Error('The workspace and its subscription did not persist in the runner database')
      return { id: workspace.id, name }
    }

    const workspace = await createWorkspace('E2E-booking-people-' + nonce)
    const schedulerWorkspace = await createWorkspace('E2E-booking-only-' + nonce)
    api.organizationID = schedulerWorkspace.id

    const role = await api.call('POST', '/api/roles', {
      name: 'Booking-only scheduler',
      // Calendar catalogues require read access. Setup mutations have a separate grant.
      permissions: ['bookings:read', 'bookings:write', 'booking.settings:read'],
    })
    const email = 'e2e-booking-only-' + nonce + '@example.test'
    const password = randomBytes(24).toString('hex')
    const user = await api.call('POST', '/api/users', {
      email, password, full_name: 'Booking-only scheduler', role_id: role.id, is_super_admin: false,
    })
    const person = await api.call('POST', '/api/booking/resources', {
      name: 'Preseeded staff member', kind: 'staff', timezone: 'Asia/Kuala_Lumpur', is_active: true,
    })
    const service = await api.call('POST', '/api/booking/services', {
      name: 'Preseeded appointment', kind: 'appointment', duration_minutes: 30, default_capacity: 2,
      price_minor: 0, currency: 'MYR', is_active: true, resource_ids: [person.id],
    })
    if (user.is_super_admin !== false || user.is_active !== true || user.organization_id !== schedulerWorkspace.id ||
        user.role_id !== role.id || !user.id || !person.id || !service.id ||
        person.organization_id !== schedulerWorkspace.id || service.organization_id !== schedulerWorkspace.id) {
      throw new Error('The booking-only fixture did not provision in the intended workspace')
    }
    return { workspace, scheduler: {
      workspace: schedulerWorkspace, userID: user.id, roleID: role.id, email, password,
      personID: person.id, serviceID: service.id,
    } }
  } finally {
    try {
      await db.end()
    } finally {
      await context.dispose()
    }
  }
}

try {
  const { workspace, scheduler } = await preseed()
  const require = createRequire(import.meta.url)
  execFileSync(process.execPath, [
    require.resolve('@playwright/test/cli'),
    'test',
    '--config=playwright.booking-people-built.config.ts',
    '--output=test-results/booking-people-built',
  ], {
    cwd: fileURLToPath(new URL('../../', import.meta.url)),
    env: {
      ...process.env,
      BOOKING_QUALIFICATION_WORKSPACE_ID: workspace.id,
      BOOKING_QUALIFICATION_WORKSPACE_NAME: workspace.name,
      BOOKING_QUALIFICATION_SCHEDULER: JSON.stringify(scheduler),
    },
    stdio: 'inherit',
    timeout: 130_000,
    windowsHide: true,
  })
} catch {
  console.error('Built booking qualification failed')
  process.exitCode = 1
}
