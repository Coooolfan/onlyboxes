<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import APIKeysModal from '@/components/account/APIKeysModal.vue'
import ChangePasswordModal from '@/components/account/ChangePasswordModal.vue'
import ConsoleSidebar from '@/components/layout/ConsoleSidebar.vue'
import UserMenu from '@/components/layout/UserMenu.vue'
import { consoleNavItems, isNavItemActive } from '@/config/navigation'
import { useAuthStore } from '@/stores/auth'
import { clearFeatureStores } from '@/stores/session'

const authStore = useAuthStore()
const route = useRoute()
const router = useRouter()

const showAPIKeysModal = ref(false)
const showChangePasswordModal = ref(false)

const currentSectionLabel = computed(() => {
  const matched = consoleNavItems.find((item) => isNavItemActive(route.path, item.to))
  return matched?.label ?? route.name?.toString() ?? 'Console'
})

async function handleLogout(): Promise<void> {
  await authStore.logout()
  clearFeatureStores()
  await router.replace('/login')
}
</script>

<template>
  <div class="flex h-screen bg-bg font-sans text-primary">
    <ConsoleSidebar />

    <div class="flex min-w-0 flex-1 flex-col overflow-hidden">
      <header
        class="z-10 flex h-16 shrink-0 items-center justify-between border-b border-stroke bg-bg px-8 max-[620px]:px-5"
      >
        <p class="m-0 text-lg font-semibold">{{ currentSectionLabel }}</p>

        <UserMenu
          @change-password="showChangePasswordModal = true"
          @api-keys="showAPIKeysModal = true"
          @logout="handleLogout"
        />
      </header>

      <main class="page-content relative flex-1 overflow-y-auto p-8 max-[620px]:p-5">
        <div class="mx-auto grid w-[min(1240px,100%)] gap-6">
          <slot />
        </div>
      </main>
    </div>

    <APIKeysModal v-if="showAPIKeysModal" @close="showAPIKeysModal = false" />
    <ChangePasswordModal v-if="showChangePasswordModal" @close="showChangePasswordModal = false" />
  </div>
</template>
