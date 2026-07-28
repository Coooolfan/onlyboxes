/** Fixed run of stars between the visible head and tail of a masked secret. */
const MASKED_MIDDLE_LENGTH = 12

/**
 * Masks a one-time secret for display, keeping only the first and last four
 * characters so users can still verify what they copied.
 *
 * The masked middle has a fixed length rather than tracking the real one: a
 * 64-character secret would otherwise render a star run too long to fit on one
 * line, and the rendered width would leak how long the secret is.
 */
export function maskSecret(secret: string): string {
  const trimmed = secret.trim()
  if (!trimmed) {
    return 'Unavailable'
  }
  if (trimmed.length <= 8) {
    return '*'.repeat(MASKED_MIDDLE_LENGTH)
  }
  return `${trimmed.slice(0, 4)}${'*'.repeat(MASKED_MIDDLE_LENGTH)}${trimmed.slice(-4)}`
}
