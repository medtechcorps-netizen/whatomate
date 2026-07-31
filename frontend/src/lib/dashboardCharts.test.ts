import { describe, expect, it } from 'vitest'
import {
  dashboardLegendLabels,
  getDashboardSeriesColor,
} from './dashboardCharts'

describe('dashboardLegendLabels', () => {
  it('uses compact circular markers', () => {
    expect(dashboardLegendLabels).toMatchObject({
      usePointStyle: true,
      pointStyle: 'circle',
      pointStyleWidth: 8,
      boxWidth: 8,
      boxHeight: 8,
      padding: 16,
    })
  })
})

describe('getDashboardSeriesColor', () => {
  it.each([
    ['won', 'rgb(16, 185, 129)'],
    ['lost', 'rgb(239, 68, 68)'],
    ['open', 'rgb(245, 158, 11)'],
    ['charge', 'rgb(139, 92, 246)'],
  ])('keeps the %s series color stable', (label, expected) => {
    expect(getDashboardSeriesColor(label, 7, ['fallback'])).toBe(expected)
    expect(getDashboardSeriesColor(label.toUpperCase(), 0, ['other'])).toBe(
      expected,
    )
  })

  it('uses a deterministic solid fallback for other series', () => {
    const palette = ['rgb(1, 2, 3)', 'rgb(4, 5, 6)']

    expect(getDashboardSeriesColor('delivered', 3, palette)).toBe(palette[1])
    expect(getDashboardSeriesColor('unknown', 0, [])).toBe(
      'rgb(59, 130, 246)',
    )
  })
})
