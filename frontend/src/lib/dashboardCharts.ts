export const dashboardLegendLabels = {
  usePointStyle: true,
  pointStyle: 'circle',
  pointStyleWidth: 8,
  boxWidth: 8,
  boxHeight: 8,
  padding: 16,
} as const

const defaultSeriesColor = 'rgb(59, 130, 246)'

const semanticSeriesColors: Record<string, string> = {
  won: 'rgb(16, 185, 129)',
  lost: 'rgb(239, 68, 68)',
  open: 'rgb(245, 158, 11)',
  charge: 'rgb(139, 92, 246)',
  charges: 'rgb(139, 92, 246)',
}

export function getDashboardSeriesColor(
  label: string,
  index: number,
  fallbackPalette: readonly string[],
): string {
  const semanticColor = semanticSeriesColors[label.trim().toLowerCase()]
  if (semanticColor) return semanticColor
  if (!fallbackPalette.length) return defaultSeriesColor

  const fallbackIndex = Math.abs(index) % fallbackPalette.length
  return fallbackPalette[fallbackIndex] ?? defaultSeriesColor
}
