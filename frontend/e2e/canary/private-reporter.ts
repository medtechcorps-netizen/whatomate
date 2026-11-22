import type { FullResult, Reporter, TestCase, TestResult, TestStep } from '@playwright/test/reporter'
import { atomicWritePrivate } from './private-files.mjs'
import { CANARY_FAILURE_PHASES, CHECK_EXECUTION_ORDER, type CanaryFailurePhase } from './checks.ts'
import { DRILL_PREPARED, DRILL_TRIGGERED, DRILL_ERROR, DRILL_EVIDENCE } from './stage-drill.mjs'

// A minimal machine report contains names/statuses only. Never serialize test
// errors, source, config, stdout, attachment paths or the private fixture.
export default class PrivateReporter implements Reporter {
  private specs: object[] = []
  private drillResults: boolean[] = []
  private runnerError = false
  private failedPhases = new WeakMap<TestResult, CanaryFailurePhase | null>()
  printsToStdio() { return true }
  onError() {
    this.runnerError = true
    console.error('Canary setup or runner failed; details suppressed because they may contain credentials')
  }
  onStepEnd(test: TestCase, result: TestResult, step: TestStep) {
    if (test.title !== CHECK_EXECUTION_ORDER[0] || step.category !== 'test.step' || !step.error) return
    const phase = Object.values(CANARY_FAILURE_PHASES).find(allowed => allowed === step.title)
    if (phase) this.failedPhases.set(result, this.failedPhases.has(result) ? null : phase)
  }
  onTestEnd(test: TestCase, result: TestResult) {
    const phase = this.failedPhases.get(result)
    if (phase && result.retry === 0 && ['failed', 'timedOut', 'interrupted'].includes(result.status)) {
      console.log(`[canary-phase] failed=${phase}`)
    }
    const index = this.specs.length
    const annotations = result.annotations ?? []
    const exactAnnotation = (type: string, description: string) =>
      annotations.filter(item => item.type === type).length === 1 &&
      annotations.some(item => item.type === type && item.description === description)
    this.drillResults.push(test.title === CHECK_EXECUTION_ORDER[index] &&
      test.expectedStatus === 'passed' && result.retry === 0 &&
      (index === 0
        ? result.status === 'failed' && result.errors.length === 1 &&
          result.errors[0].message === `Error: ${DRILL_ERROR}` &&
          exactAnnotation(DRILL_PREPARED, 'complete') && exactAnnotation(DRILL_TRIGGERED, 'e2e-fail')
        : result.status === 'skipped' && result.errors.length === 0))
    console.log(`[canary] ${test.title}: ${result.status}`)
    this.specs.push({ title: test.title, tests: [{ expectedStatus: test.expectedStatus,
      status: test.outcome(), results: [{ status: result.status, retry: result.retry }] }] })
  }
  onEnd(result: FullResult) {
    console.log(`[canary] ${this.specs.length} checks: ${result.status}`)
    if (process.env.CANARY_REPORT_FILE) atomicWritePrivate(process.env.CANARY_REPORT_FILE,
      JSON.stringify({ specs: this.specs, errors: result.status === 'passed' ? [] : ['Canary run failed'] }))
    // Only trusted hook annotations plus the exact throw establish the drill's
    // cause. Wait for teardown/global errors before emitting affirmative evidence.
    if (process.env.CANARY_REPORT_FILE &&
        process.env.CANARY_PROFILE === 'staging' && process.env.CANARY_DRILL === 'e2e-fail' &&
        process.env.CANARY_STRESS !== '1' && result.status === 'failed' && !this.runnerError &&
        this.drillResults.length === CHECK_EXECUTION_ORDER.length && this.drillResults.every(Boolean)) {
      console.log(DRILL_EVIDENCE)
    }
  }
}
