import { CHECK_EXECUTION_ORDER } from './checks.ts'

export function canaryDrill(env = process.env) {
  const drill = env.CANARY_DRILL ?? 'none'
  if (!['none', 'e2e-fail'].includes(drill) ||
      (drill !== 'none' && (env.CANARY_PROFILE !== 'staging' || env.CANARY_STRESS === '1'))) {
    throw new Error('Canary drill is allowed only for the staging thirteen-check run')
  }
  return drill
}

export function applyCanaryDrill(name, env = process.env) {
  if (canaryDrill(env) === 'e2e-fail' && name === CHECK_EXECUTION_ORDER[0]) {
    throw new Error('Intentional staging canary failure drill')
  }
}
