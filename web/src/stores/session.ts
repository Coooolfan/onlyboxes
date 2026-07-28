import { useAccountsStore } from '@/stores/accounts'
import { useAPIKeysStore } from '@/stores/apiKeys'
import { useTokensStore } from '@/stores/tokens'
import { useWorkersStore } from '@/stores/workers'

/**
 * Aborts in-flight requests and clears cached data for every feature store.
 * Called on logout so a different account never sees the previous session state.
 */
export function clearFeatureStores(): void {
  for (const store of [useAccountsStore(), useAPIKeysStore(), useWorkersStore(), useTokensStore()]) {
    store.teardown()
    store.reset()
  }
}
