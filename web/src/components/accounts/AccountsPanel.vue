<script setup lang="ts">
import AppBadge from '@/components/ui/AppBadge.vue'
import AppButton from '@/components/ui/AppButton.vue'
import AppPagination from '@/components/ui/AppPagination.vue'
import { accountPageSize } from '@/stores/accounts'
import { formatDateTime } from '@/utils/datetime'
import type { AccountListItem } from '@/types/auth'

defineProps<{
  accounts: AccountListItem[]
  total: number
  page: number
  totalPages: number
  loading: boolean
  currentAccountId: string
  deletingAccountId: string
}>()

const emit = defineEmits<{
  prevPage: []
  nextPage: []
  deleteAccount: [accountID: string]
}>()
</script>

<template>
  <section
    class="account-panel ui-rise overflow-hidden rounded-lg border border-stroke bg-surface shadow-card max-[620px]:rounded-default"
  >
    <header class="border-b border-stroke bg-surface-soft px-6 py-5">
      <h2 class="m-0 text-lg font-semibold">Accounts</h2>
      <p class="m-0 mt-1 text-sm text-secondary">{{ total }} accounts in total</p>
    </header>

    <div class="overflow-x-auto">
      <table class="w-full min-w-[720px] border-collapse">
        <thead>
          <tr>
            <th scope="col" class="ui-th">Username</th>
            <th scope="col" class="ui-th">Role</th>
            <th scope="col" class="ui-th">Created</th>
            <th scope="col" class="ui-th">Updated</th>
            <th scope="col" class="ui-th text-right">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="accounts.length === 0">
            <td colspan="5" class="ui-td text-center text-secondary">
              <span class="block py-8">No accounts found on this page.</span>
            </td>
          </tr>

          <tr
            v-for="item in accounts"
            :key="item.account_id"
            class="transition-colors duration-200 hover:bg-surface-soft"
          >
            <td class="ui-td">
              <div class="flex items-center gap-2">
                <span class="font-medium">{{ item.username }}</span>
                <AppBadge v-if="item.account_id === currentAccountId" pill>Current</AppBadge>
              </div>
              <p class="m-0 mt-1 font-mono text-xs text-secondary">{{ item.account_id }}</p>
            </td>
            <td class="ui-td">
              <AppBadge :tone="item.is_admin ? 'admin' : 'member'" pill>
                {{ item.is_admin ? 'Admin' : 'Member' }}
              </AppBadge>
            </td>
            <td class="ui-td text-secondary">{{ formatDateTime(item.created_at) }}</td>
            <td class="ui-td text-secondary">{{ formatDateTime(item.updated_at) }}</td>
            <td class="ui-td text-right">
              <AppButton
                v-if="!item.is_admin && item.account_id !== currentAccountId"
                class="account-delete-btn"
                variant="danger"
                size="sm"
                icon="trash"
                :loading="deletingAccountId === item.account_id"
                @click="emit('deleteAccount', item.account_id)"
              >
                {{ deletingAccountId === item.account_id ? 'Deleting...' : 'Delete' }}
              </AppButton>
              <span v-else class="text-xs text-secondary">Protected</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div class="border-t border-stroke bg-surface-soft">
      <AppPagination
        :page="page"
        :total-pages="totalPages"
        :total="total"
        :page-size="accountPageSize"
        :loading="loading"
        item-label="accounts"
        @prev="emit('prevPage')"
        @next="emit('nextPage')"
      />
    </div>
  </section>
</template>
