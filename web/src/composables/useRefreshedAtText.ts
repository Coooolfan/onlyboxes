import { computed, type ComputedRef, type Ref } from 'vue'

import { formatDateTime } from '@/utils/datetime'

/**
 * Formats a store refresh timestamp for the page header, shared by every
 * dashboard page so the "Last refresh" wording stays identical.
 */
export function useRefreshedAtText(refreshedAt: Ref<Date | null>): ComputedRef<string> {
  return computed(() => {
    if (!refreshedAt.value) {
      return 'never'
    }
    return formatDateTime(refreshedAt.value.toISOString())
  })
}
