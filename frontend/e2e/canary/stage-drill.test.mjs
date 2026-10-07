import assert from 'node:assert/strict'
import test from 'node:test'
import { CHECK_EXECUTION_ORDER } from './checks.ts'
import { canaryDrill, applyCanaryDrill } from './stage-drill.mjs'

test('only staging normal-run e2e-fail is permitted', () => {
  assert.equal(canaryDrill({ CANARY_PROFILE: 'local' }), 'none')
  assert.equal(canaryDrill({ CANARY_PROFILE: 'staging', CANARY_DRILL: 'e2e-fail' }), 'e2e-fail')
  for (const env of [
    { CANARY_PROFILE: 'local', CANARY_DRILL: 'e2e-fail' },
    { CANARY_PROFILE: 'staging', CANARY_DRILL: 'health-fail' },
    { CANARY_PROFILE: 'staging', CANARY_DRILL: 'bad-image' },
    { CANARY_PROFILE: 'staging', CANARY_DRILL: 'e2e-fail', CANARY_STRESS: '1' },
    { CANARY_DRILL: 'e2e-fail' },
  ]) assert.throws(() => canaryDrill(env))
})

test('first check deliberately fails with a fixed credential-free error', () => {
  const env = { CANARY_PROFILE: 'staging', CANARY_DRILL: 'e2e-fail' }
  assert.throws(() => applyCanaryDrill(CHECK_EXECUTION_ORDER[0], env), /^Error: Intentional staging canary failure drill$/)
  for (const name of CHECK_EXECUTION_ORDER.slice(1)) assert.doesNotThrow(() => applyCanaryDrill(name, env))
  for (const name of CHECK_EXECUTION_ORDER) assert.doesNotThrow(() => applyCanaryDrill(name, { CANARY_PROFILE: 'staging' }))
})
