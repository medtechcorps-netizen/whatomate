const DEFAULT_CURRENCY_FRACTION_DIGITS = 2

export function formatCurrencyMinorUnits(
  currency: string,
  amountMinor: number,
  locale = 'en-MY',
): string {
  const normalizedCurrency = currency.trim().toUpperCase()

  try {
    const formatter = new Intl.NumberFormat(locale, {
      style: 'currency',
      currency: normalizedCurrency,
    })
    const fractionDigits = formatter.resolvedOptions().maximumFractionDigits
    return formatter.format(amountMinor / 10 ** fractionDigits)
  } catch {
    return `${currency} ${(amountMinor / 10 ** DEFAULT_CURRENCY_FRACTION_DIGITS).toFixed(
      DEFAULT_CURRENCY_FRACTION_DIGITS,
    )}`
  }
}
