<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

import ErrorBanner from '@/components/common/ErrorBanner.vue'
import AccountsPanel from '@/components/dashboard/AccountsPanel.vue'
import ConsoleHeader from '@/components/dashboard/ConsoleHeader.vue'
import CreateAccountModal from '@/components/dashboard/CreateAccountModal.vue'
import { useAccountsStore } from '@/stores/accounts'
import { useAuthStore } from '@/stores/auth'

const authStore = useAuthStore()
const accountsStore = useAccountsStore()

const showCreateAccountModal = ref(false)

const refreshedAtText = computed(() => {
  if (!accountsStore.refreshedAt) {
    return 'never'
  }
  return accountsStore.formatDateTime(accountsStore.refreshedAt.toISOString())
})

const showCreateAccountPanel = computed(() => authStore.isAdmin && authStore.registrationEnabled)

async function handleRefresh(): Promise<void> {
  await accountsStore.loadAccounts(accountsStore.page)
}

function openCreateAccountModal(): void {
  showCreateAccountModal.value = true
}

function closeCreateAccountModal(): void {
  showCreateAccountModal.value = false
}

onMounted(async () => {
  await accountsStore.loadAccounts(accountsStore.page)
})

onBeforeUnmount(() => {
  accountsStore.teardown()
})
</script>

<template>
  <main class="relative z-2 mx-auto w-[min(1240px,100%)] grid gap-6">
    <ConsoleHeader
      eyebrow="Onlyboxes / Account Console"
      title="Account Administration"
      :loading="accountsStore.loading"
      :refreshed-at-text="refreshedAtText"
      @refresh="handleRefresh"
    >
      <template #subtitle>
        Account: <strong>{{ authStore.currentAccount?.username ?? '--' }}</strong>
      </template>
      <template #actions>
        <button
          v-if="showCreateAccountPanel"
          class="ui-btn-secondary rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
          type="button"
          @click="openCreateAccountModal"
        >
          Create Account
        </button>
      </template>
    </ConsoleHeader>

    <ErrorBanner v-if="accountsStore.errorMessage" :message="accountsStore.errorMessage" />

    <AccountsPanel
      v-if="authStore.isAdmin"
      :accounts="accountsStore.accounts"
      :total="accountsStore.total"
      :page="accountsStore.page"
      :total-pages="accountsStore.totalPages"
      :can-prev="accountsStore.canPrev"
      :can-next="accountsStore.canNext"
      :footer-text="accountsStore.footerText"
      :loading="accountsStore.loading"
      :current-account-id="authStore.currentAccount?.account_id ?? ''"
      :deleting-account-id="accountsStore.deletingAccountID"
      :delete-button-text="accountsStore.deleteAccountButtonText"
      :format-date-time="accountsStore.formatDateTime"
      @prev-page="accountsStore.previousPage"
      @next-page="accountsStore.nextPage"
      @delete-account="accountsStore.deleteAccount"
    />

    <CreateAccountModal v-if="showCreateAccountModal" @close="closeCreateAccountModal" />
  </main>
</template>
