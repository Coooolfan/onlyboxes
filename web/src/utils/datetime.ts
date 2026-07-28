const dateTimeFormatter = new Intl.DateTimeFormat('en-US', {
  month: 'short',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
})

export function formatDateTime(value: string): string {
  const parsed = Date.parse(value)
  if (Number.isNaN(parsed)) {
    return '--'
  }
  return dateTimeFormatter.format(new Date(parsed))
}

/** Seconds elapsed since `value`, or `Infinity` when the timestamp is unusable. */
export function ageSeconds(value: string): number {
  const parsed = Date.parse(value)
  if (Number.isNaN(parsed)) {
    return Number.POSITIVE_INFINITY
  }
  return Math.max(0, Math.floor((Date.now() - parsed) / 1000))
}

export function formatAge(value: string): string {
  const seconds = ageSeconds(value)
  if (!Number.isFinite(seconds)) {
    return '--'
  }
  if (seconds < 60) {
    return `${seconds}s ago`
  }

  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) {
    return `${minutes}m ago`
  }

  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return `${hours}h ago`
  }

  return `${Math.floor(hours / 24)}d ago`
}
