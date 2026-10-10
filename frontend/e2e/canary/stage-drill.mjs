import process from 'node:process'
import { CHECK_EXECUTION_ORDER } from './checks.ts'

export const DRILL_PREPARED = 'canary-preparation-complete'
export const DRILL_TRIGGERED = 'canary-intentional-drill'
export const DRILL_ERROR = 'Intentional staging canary failure drill'
export const DRILL_EVIDENCE = '[canary-drill] e2e-fail: preparation complete; intentional failure observed'

export function canaryDrill(env = process.env) {
  const drill = env.CANARY_DRILL ?? 'none'
  if (!['none', 'e2e-fail'].includes(drill) ||
      (drill !== 'none' && (env.CANARY_PROFILE !== 'staging' || env.CANARY_STRESS === '1'))) {
    throw new Error('Canary drill is allowed only for the staging thirteen-check run')
  }
  return drill
}

export function applyCanaryDrill(name, env = process.env, recordTrigger = () => {}) {
  if (canaryDrill(env) === 'e2e-fail' && name === CHECK_EXECUTION_ORDER[0]) {
    recordTrigger()
    throw new Error(DRILL_ERROR)
  }
}
