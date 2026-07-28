/**
 * Masks a one-time secret for display, keeping only the first and last four
 * characters so users can still verify what they copied.
 */
export function maskSecret(secret: string): string {
  const trimmed = secret.trim()
  if (!trimmed) {
    return 'Unavailable'
  }
  if (trimmed.length <= 8) {
    return '*'.repeat(trimmed.length)
  }
  const middleMaskLength = Math.max(4, trimmed.length - 8)
  return `${trimmed.slice(0, 4)}${'*'.repeat(middleMaskLength)}${trimmed.slice(-4)}`
}
