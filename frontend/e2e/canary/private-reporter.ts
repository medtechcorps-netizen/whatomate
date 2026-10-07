import type { FullResult, Reporter, TestCase, TestResult } from '@playwright/test/reporter'
import { atomicWritePrivate } from './private-files.mjs'

// A minimal machine report contains names/statuses only. Never serialize test
// errors, source, config, stdout, attachment paths or the private fixture.
export default class PrivateReporter implements Reporter {
  private specs: object[] = []
  printsToStdio() { return true }
  onError() { console.error('Canary setup or runner failed; details suppressed because they may contain credentials') }
  onTestEnd(test: TestCase, result: TestResult) {
    console.log(`[canary] ${test.title}: ${result.status}`)
    this.specs.push({ title: test.title, tests: [{ expectedStatus: test.expectedStatus,
      status: test.outcome(), results: [{ status: result.status, retry: result.retry }] }] })
  }
  onEnd(result: FullResult) {
    console.log(`[canary] ${this.specs.length} checks: ${result.status}`)
    if (process.env.CANARY_REPORT_FILE) atomicWritePrivate(process.env.CANARY_REPORT_FILE,
      JSON.stringify({ specs: this.specs, errors: result.status === 'passed' ? [] : ['Canary run failed'] }))
  }
}
