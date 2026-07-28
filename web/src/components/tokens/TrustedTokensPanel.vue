<script setup lang="ts">
import { ref } from 'vue'

import TokenCreateModal from '@/components/tokens/TokenCreateModal.vue'
import AppButton from '@/components/ui/AppButton.vue'
import AppEmptyState from '@/components/ui/AppEmptyState.vue'
import DefinitionRow from '@/components/ui/DefinitionRow.vue'
import { formatDateTime } from '@/utils/datetime'
import type { TrustedTokenItem } from '@/types/workers'

defineProps<{
  tokens: TrustedTokenItem[]
  deletingTokenId: string
}>()

const emit = defineEmits<{
  deleteToken: [tokenID: string]
}>()

const showCreateModal = ref(false)
</script>

<template>
  <section
    class="token-panel ui-rise rounded-lg border border-stroke bg-surface p-6 shadow-card max-[620px]:p-5"
  >
    <div class="flex items-start justify-between gap-4 max-[700px]:flex-col">
      <div>
        <h2 class="m-0 text-lg font-semibold">Trusted Tokens</h2>
        <p class="m-0 mt-1 text-sm text-secondary">Total: {{ tokens.length }}</p>
      </div>
      <AppButton variant="primary" icon="plus" @click="showCreateModal = true">
        New Token
      </AppButton>
    </div>

    <div class="pt-5">
      <AppEmptyState
        v-if="tokens.length === 0"
        icon="key"
        dashed
        title="No trusted tokens configured"
        description="MCP access can still use JIT auth when it is enabled."
      />

      <ul v-else class="m-0 grid list-none gap-3 p-0">
        <li
          v-for="item in tokens"
          :key="item.id"
          class="flex items-start justify-between gap-4 rounded-lg border border-stroke bg-surface px-5 py-4 transition-[box-shadow,border-color] duration-200 hover:border-stroke-hover hover:shadow-card-hover max-[700px]:flex-col"
        >
          <div class="grid min-w-0 gap-2">
            <p class="m-0 text-[15px] font-semibold text-primary">{{ item.name }}</p>
            <DefinitionRow label="ID" :value="item.id" label-width="64px" />
            <DefinitionRow label="Masked" :value="item.token_masked" label-width="64px" />
            <DefinitionRow
              label="Created"
              :value="formatDateTime(item.created_at)"
              label-width="64px"
              :code="false"
            />
          </div>

          <div class="token-actions shrink-0">
            <AppButton
              variant="danger"
              size="sm"
              icon="trash"
              :loading="deletingTokenId === item.id"
              @click="emit('deleteToken', item.id)"
            >
              {{ deletingTokenId === item.id ? 'Deleting...' : 'Delete' }}
            </AppButton>
          </div>
        </li>
      </ul>
    </div>

    <TokenCreateModal v-if="showCreateModal" @close="showCreateModal = false" />
  </section>
</template>
