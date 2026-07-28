<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

import AccountsPanel from '@/components/accounts/AccountsPanel.vue'
import CreateAccountModal from '@/components/accounts/CreateAccountModal.vue'
import PageHeader from '@/components/layout/PageHeader.vue'
import AppAlert from '@/components/ui/AppAlert.vue'
import AppButton from '@/components/ui/AppButton.vue'
import { useRefreshedAtText } from '@/composables/useRefreshedAtText'
import { useAccountsStore } from '@/stores/accounts'
import { useAuthStore } from '@/stores/auth'

const authStore = useAuthStore()
const accountsStore = useAccountsStore()

const showCreateAccountModal = ref(false)

const refreshedAtText = useRefreshedAtText(computed(() => accountsStore.refreshedAt))
const canCreateAccount = computed(() => authStore.isAdmin && authStore.registrationEnabled)

async function refresh(): Promise<void> {
  await accountsStore.loadAccounts(accountsStore.page)
}

onMounted(refresh)
onBeforeUnmount(() => {
  accountsStore.teardown()
})
</script>

<template>
  <div class="grid gap-6">
    <PageHeader
      eyebrow="Onlyboxes / Account Console"
      title="Account Administration"
      :loading="accountsStore.loading"
      :refreshed-at-text="refreshedAtText"
      @refresh="refresh"
    >
      <template #subtitle>
        Account: <strong>{{ authStore.currentAccount?.username ?? '--' }}</strong>
      </template>
      <template #actions>
        <AppButton v-if="canCreateAccount" icon="plus" @click="showCreateAccountModal = true">
          Create Account
        </AppButton>
      </template>
    </PageHeader>

    <AppAlert v-if="accountsStore.errorMessage" tone="error" with-icon>
      {{ accountsStore.errorMessage }}
    </AppAlert>

    <AccountsPanel
      v-if="authStore.isAdmin"
      :accounts="accountsStore.accounts"
      :total="accountsStore.total"
      :page="accountsStore.page"
      :total-pages="accountsStore.totalPages"
      :loading="accountsStore.loading"
      :current-account-id="authStore.currentAccount?.account_id ?? ''"
      :deleting-account-id="accountsStore.deletingAccountID"
      @prev-page="accountsStore.previousPage"
      @next-page="accountsStore.nextPage"
      @delete-account="accountsStore.deleteAccount"
    />

    <CreateAccountModal
      v-if="showCreateAccountModal"
      @close="showCreateAccountModal = false"
      @created="refresh"
    />
  </div>
</template>
