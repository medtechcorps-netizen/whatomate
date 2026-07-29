import { describe, expect, it } from 'vitest'
import { formatCurrencyMinorUnits } from './currency'

describe('formatCurrencyMinorUnits', () => {
  it.each([
    ['JPY', 1234, 1234],
    ['MYR', 1234, 12.34],
    ['BHD', 1234, 1.234],
  ])(
    'uses the fraction digits defined for %s',
    (currency, amountMinor, amountMajor) => {
      const expected = new Intl.NumberFormat('en-US', {
        style: 'currency',
        currency,
      }).format(amountMajor)

      expect(formatCurrencyMinorUnits(currency, amountMinor, 'en-US')).toBe(
        expected,
      )
    },
  )

  it('falls back to two fraction digits for an invalid currency code', () => {
    expect(formatCurrencyMinorUnits('credits', 1234, 'en-US')).toBe(
      'credits 12.34',
    )
  })
})
