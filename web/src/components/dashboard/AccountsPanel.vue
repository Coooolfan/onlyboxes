<script setup lang="ts">
import type { AccountListItem } from '@/types/auth'

defineProps<{
  accounts: AccountListItem[]
  total: number
  page: number
  totalPages: number
  canPrev: boolean
  canNext: boolean
  footerText: string
  loading: boolean
  currentAccountId: string
  deletingAccountId: string
  deleteButtonText: (accountID: string) => string
  formatDateTime: (value: string) => string
}>()

const emit = defineEmits<{
  prevPage: []
  nextPage: []
  deleteAccount: [accountID: string]
}>()
</script>

<template>
  <section
    class="account-panel border border-stroke rounded-lg bg-surface shadow-card overflow-hidden animate-[rise-in_620ms_ease-out] max-[620px]:rounded-default"
  >
    <div class="px-6 py-5 border-b border-stroke bg-surface-soft">
      <div>
        <h2 class="m-0 text-lg font-semibold">Accounts</h2>
        <p class="mt-1 mb-0 text-secondary text-sm">{{ footerText }} accounts</p>
      </div>
    </div>

    <div class="overflow-x-auto">
      <table class="min-w-full border-collapse text-sm">
        <thead class="bg-surface-soft text-secondary">
          <tr>
            <th class="text-left font-medium px-6 py-3 border-b border-stroke">Username</th>
            <th class="text-left font-medium px-6 py-3 border-b border-stroke">Role</th>
            <th class="text-left font-medium px-6 py-3 border-b border-stroke">Created</th>
            <th class="text-left font-medium px-6 py-3 border-b border-stroke">Updated</th>
            <th class="text-right font-medium px-6 py-3 border-b border-stroke">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="accounts.length === 0">
            <td colspan="5" class="px-6 py-8 text-center text-secondary">
              No accounts found on this page.
            </td>
          </tr>
          <tr
            v-for="item in accounts"
            :key="item.account_id"
            class="border-b border-stroke last:border-b-0"
          >
            <td class="px-6 py-3 text-primary">
              <div class="flex items-center gap-2">
                <span>{{ item.username }}</span>
                <span
                  v-if="item.account_id === currentAccountId"
                  class="ui-badge-soft inline-flex items-center rounded-full border px-2 py-0.5 text-[11px]"
                >
                  Current
                </span>
              </div>
              <p class="m-0 mt-1 text-[12px] text-secondary font-mono">{{ item.account_id }}</p>
            </td>
            <td class="px-6 py-3 text-primary">
              <span
                class="inline-flex items-center rounded-full border px-2.5 py-1 text-xs font-medium"
                :class="item.is_admin ? 'ui-role-badge-admin' : 'ui-role-badge-member'"
              >
                {{ item.is_admin ? 'Admin' : 'Member' }}
              </span>
            </td>
            <td class="px-6 py-3 text-secondary">{{ formatDateTime(item.created_at) }}</td>
            <td class="px-6 py-3 text-secondary">{{ formatDateTime(item.updated_at) }}</td>
            <td class="px-6 py-3 text-right">
              <button
                v-if="!item.is_admin && item.account_id !== currentAccountId"
                type="button"
                class="ui-btn-danger account-delete-btn rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
                :disabled="deletingAccountId === item.account_id"
                @click="emit('deleteAccount', item.account_id)"
              >
                {{ deleteButtonText(item.account_id) }}
              </button>
              <span v-else class="text-secondary text-xs">Protected</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div
      class="flex items-center justify-between gap-3 px-6 py-4 border-t border-stroke bg-surface-soft max-[700px]:flex-col max-[700px]:items-stretch"
    >
      <p class="m-0 text-secondary text-sm">
        Total: {{ total }} • Page {{ page }} / {{ totalPages }}
      </p>
      <div class="flex items-center gap-2">
        <button
          type="button"
          class="ui-btn-secondary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
          :disabled="!canPrev || loading"
          @click="emit('prevPage')"
        >
          Prev
        </button>
        <button
          type="button"
          class="ui-btn-secondary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
          :disabled="!canNext || loading"
          @click="emit('nextPage')"
        >
          Next
        </button>
      </div>
    </div>
  </section>
</template>
