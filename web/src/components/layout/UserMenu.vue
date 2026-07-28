<script setup lang="ts">
import { computed, ref } from 'vue'

import AppIcon from '@/components/ui/AppIcon.vue'
import { useDismissibleMenu } from '@/composables/useDismissibleMenu'
import { useAuthStore } from '@/stores/auth'

const emit = defineEmits<{
  changePassword: []
  apiKeys: []
  logout: []
}>()

const authStore = useAuthStore()

const containerRef = ref<HTMLElement | null>(null)
const open = ref(false)

const username = computed(() => authStore.currentAccount?.username ?? '')
const initial = computed(() => username.value.charAt(0).toUpperCase() || 'U')
const roleLabel = computed(() => (authStore.isAdmin ? 'Administrator' : 'User'))

useDismissibleMenu({
  containerRef,
  isOpen: open,
  onClose: () => {
    open.value = false
  },
})

type MenuAction = 'changePassword' | 'apiKeys' | 'logout'

function emitAndClose(action: MenuAction): void {
  open.value = false
  if (action === 'changePassword') {
    emit('changePassword')
    return
  }
  if (action === 'apiKeys') {
    emit('apiKeys')
    return
  }
  emit('logout')
}
</script>

<template>
  <div ref="containerRef" class="relative">
    <button
      type="button"
      class="ui-focusable flex items-center gap-2 rounded-full p-1.5 transition-colors hover:bg-surface-soft"
      aria-haspopup="menu"
      :aria-expanded="open"
      :aria-label="`Account menu for ${username}`"
      @click="open = !open"
    >
      <span
        class="ui-on-accent flex h-8 w-8 items-center justify-center rounded-full bg-accent text-sm font-bold"
        aria-hidden="true"
      >
        {{ initial }}
      </span>
    </button>

    <Transition name="menu">
      <div
        v-if="open"
        class="absolute right-0 z-50 mt-2 w-56 origin-top-right rounded-md border border-stroke bg-surface py-1 shadow-modal"
        role="menu"
      >
        <div class="border-b border-stroke/50 px-4 py-2">
          <p class="m-0 truncate text-sm font-medium text-primary">{{ username }}</p>
          <p class="m-0 truncate text-xs text-secondary">{{ roleLabel }}</p>
        </div>

        <button
          type="button"
          role="menuitem"
          class="flex w-full items-center gap-2 px-4 py-2 text-left text-sm whitespace-nowrap text-primary transition-colors hover:bg-surface-soft"
          @click="emitAndClose('changePassword')"
        >
          <AppIcon name="lock" :size="14" />
          Change Password
        </button>
        <button
          type="button"
          role="menuitem"
          class="flex w-full items-center gap-2 px-4 py-2 text-left text-sm whitespace-nowrap text-primary transition-colors hover:bg-surface-soft"
          @click="emitAndClose('apiKeys')"
        >
          <AppIcon name="key" :size="14" />
          API Keys
        </button>
        <button
          type="button"
          role="menuitem"
          class="flex w-full items-center gap-2 px-4 py-2 text-left text-sm whitespace-nowrap text-offline transition-colors hover:bg-offline/10"
          @click="emitAndClose('logout')"
        >
          <AppIcon name="logout" :size="14" />
          Logout
        </button>
      </div>
    </Transition>
  </div>
</template>
