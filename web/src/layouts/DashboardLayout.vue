<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import ChangePasswordModal from '@/components/dashboard/ChangePasswordModal.vue'
import AppIcon from '@/components/icons/AppIcon.vue'
import { useAccountsStore } from '@/stores/accounts'
import { useAuthStore } from '@/stores/auth'
import { useTokensStore } from '@/stores/tokens'
import { useWorkersStore } from '@/stores/workers'

const authStore = useAuthStore()
const accountsStore = useAccountsStore()
const workersStore = useWorkersStore()
const tokensStore = useTokensStore()

const route = useRoute()
const router = useRouter()

const showUserMenu = ref(false)
const showChangePasswordModal = ref(false)
const sidebarCollapsed = ref(false)
const sidebarStateStorageKey = 'onlyboxes.sidebar.collapsed'

if (typeof window !== 'undefined') {
  sidebarCollapsed.value = window.localStorage.getItem(sidebarStateStorageKey) === '1'
}

type NavItem = {
  label: string
  to: string
  icon: 'box' | 'users' | 'key' | 'sliders'
  requiresAdmin?: boolean
}

const navItems: NavItem[] = [
  { label: 'Workers', to: '/workers', icon: 'box' },
  { label: 'Accounts', to: '/accounts', icon: 'users', requiresAdmin: true },
  { label: 'Tokens', to: '/tokens', icon: 'key' },
  { label: 'Worker Tool', to: '/tools/worker-startup', icon: 'sliders' },
]

const filteredNavItems = computed(() => {
  return navItems.filter((item) => !item.requiresAdmin || authStore.isAdmin)
})

function isNavItemActive(path: string): boolean {
  return route.path === path || route.path.startsWith(`${path}/`)
}

const currentRouteName = computed(() => {
  const matched = filteredNavItems.value.find((item) => isNavItemActive(item.to))
  return matched ? matched.label : route.name?.toString() || 'Console'
})

function toggleUserMenu() {
  showUserMenu.value = !showUserMenu.value
}

function closeUserMenu() {
  showUserMenu.value = false
}

function openChangePasswordModal() {
  closeUserMenu()
  showChangePasswordModal.value = true
}

function closeChangePasswordModal() {
  showChangePasswordModal.value = false
}

function toggleSidebar() {
  // sidebarCollapsed.value = !sidebarCollapsed.value
}

function handleCollapsedSidebarHeaderClick() {
  if (!sidebarCollapsed.value) {
    return
  }
  toggleSidebar()
}

async function handleLogout() {
  closeUserMenu()
  await authStore.logout()

  accountsStore.teardown()
  accountsStore.reset()
  workersStore.teardown()
  workersStore.reset()
  tokensStore.teardown()
  tokensStore.reset()

  await router.replace('/login')
}

watch(sidebarCollapsed, (collapsed) => {
  if (typeof window === 'undefined') {
    return
  }
  window.localStorage.setItem(sidebarStateStorageKey, collapsed ? '1' : '0')
})
</script>

<template>
  <div class="flex h-screen bg-bg text-primary font-sans">
    <!-- Sidebar -->
    <aside
      :class="[
        'border-r border-stroke flex flex-col bg-surface-soft/30 shrink-0 transition-[width] duration-300 ease-in-out',
        sidebarCollapsed ? 'w-16' : 'w-64',
      ]"
    >
      <div class="h-16 border-b border-stroke/50 relative">
        <div
          class="h-full flex items-center transition-[padding] duration-200 ease-out"
          :class="
            sidebarCollapsed
              ? 'justify-center px-0 cursor-pointer hover:bg-surface-soft/70'
              : 'justify-start px-3 pr-10 gap-2'
          "
          @click="handleCollapsedSidebarHeaderClick"
        >
          <img
            src="/onlyboxes.avif"
            alt="Onlyboxes"
            class="w-8 h-8 rounded-md object-cover shrink-0"
          />
          <span
            class="font-bold text-lg tracking-tight whitespace-nowrap overflow-hidden transition-[max-width,opacity,margin] duration-200 ease-out"
            :class="
              sidebarCollapsed ? 'max-w-0 opacity-0 ml-0' : 'max-w-[140px] opacity-100 ml-0.5'
            "
          >
            Onlyboxes
          </span>
        </div>

        <div v-if="!sidebarCollapsed" class="absolute right-2 top-1/2 -translate-y-1/2 group">
          <button
            type="button"
            class="rounded-md p-1.5 text-secondary hover:text-primary hover:bg-surface-soft transition-colors"
            aria-label="Collapse sidebar"
            @click="toggleSidebar"
          >
            <AppIcon name="chevron-left" :size="16" />
          </button>

          <div
            class="pointer-events-none absolute right-0 top-[calc(100%+8px)] z-20 whitespace-nowrap rounded-default border border-stroke bg-surface px-2.5 py-1.5 text-xs text-secondary shadow-card opacity-0 translate-y-1 transition-all duration-150 ease-out group-hover:opacity-100 group-hover:translate-y-0"
          >
            Unavailable in the open-source edition.
          </div>
        </div>
      </div>

      <nav class="flex-1 overflow-y-auto p-2.5 space-y-1">
        <RouterLink
          v-for="item in filteredNavItems"
          :key="item.to"
          :to="item.to"
          :title="sidebarCollapsed ? item.label : undefined"
          :aria-label="sidebarCollapsed ? item.label : undefined"
          class="h-10 flex items-center rounded-md text-sm font-medium transition-colors"
          :class="[
            sidebarCollapsed ? 'justify-center px-0' : 'justify-start px-3',
            isNavItemActive(item.to)
              ? 'bg-accent ui-on-accent'
              : 'text-secondary hover:text-primary hover:bg-surface-soft',
          ]"
        >
          <AppIcon :name="item.icon" :size="18" />
          <span
            class="overflow-hidden whitespace-nowrap transition-[max-width,opacity,margin] duration-200 ease-out"
            :class="sidebarCollapsed ? 'max-w-0 opacity-0 ml-0' : 'max-w-[120px] opacity-100 ml-2'"
          >
            {{ item.label }}
          </span>
        </RouterLink>
      </nav>

      <div
        :class="[
          'border-t border-stroke/50 text-xs text-secondary font-mono',
          sidebarCollapsed
            ? 'p-3 flex justify-center'
            : 'p-3.5 flex items-center justify-between gap-2',
        ]"
      >
        <span
          class="whitespace-nowrap overflow-hidden transition-[max-width,opacity,margin] duration-200 ease-out"
          :class="sidebarCollapsed ? 'max-w-0 opacity-0 mr-0' : 'max-w-[160px] opacity-100 mr-1'"
        >
          Console {{ authStore.consoleVersion }}
        </span>
        <a
          :href="authStore.consoleRepoURL"
          target="_blank"
          rel="noopener noreferrer"
          :title="`Console ${authStore.consoleVersion} · GitHub`"
          aria-label="Onlyboxes Console GitHub"
          class="console-footer-link inline-flex items-center justify-center rounded-md p-1.5 text-secondary hover:text-primary hover:bg-surface-soft transition-colors shrink-0"
        >
          <AppIcon name="github" :size="16" />
        </a>
      </div>
    </aside>

    <!-- Main Content -->
    <div class="flex-1 flex flex-col min-w-0 overflow-hidden">
      <!-- TopBar -->
      <header
        class="h-16 flex items-center justify-between px-8 border-b border-stroke bg-bg z-10"
      >
        <h1 class="text-lg font-semibold">{{ currentRouteName }}</h1>

        <div class="relative">
          <button
            class="flex items-center gap-2 hover:bg-surface-soft p-1.5 rounded-full transition-colors outline-none focus:ring-2 focus:ring-accent/20"
            @click="toggleUserMenu"
          >
            <div
              class="w-8 h-8 rounded-full bg-accent ui-on-accent flex items-center justify-center text-sm font-bold"
            >
              {{ authStore.currentAccount?.username?.charAt(0).toUpperCase() || 'U' }}
            </div>
          </button>

          <!-- Dropdown -->
          <div
            v-if="showUserMenu"
            class="absolute right-0 mt-2 w-48 bg-surface border border-stroke rounded-md shadow-card py-1 z-50 animate-in fade-in zoom-in-95 duration-100"
          >
            <div class="px-4 py-2 border-b border-stroke/50">
              <p class="text-sm font-medium text-primary truncate">
                {{ authStore.currentAccount?.username }}
              </p>
              <p class="text-xs text-secondary truncate">
                {{ authStore.isAdmin ? 'Administrator' : 'User' }}
              </p>
            </div>
            <button
              class="w-full text-left px-4 py-2 text-sm text-primary hover:bg-surface-soft transition-colors flex items-center gap-2"
              @click="openChangePasswordModal"
            >
              <AppIcon name="lock" :size="14" />
              Change Password
            </button>
            <button
              class="w-full text-left px-4 py-2 text-sm text-offline hover:bg-offline/10 transition-colors flex items-center gap-2"
              @click="handleLogout"
            >
              <AppIcon name="logout" :size="14" />
              Logout
            </button>
          </div>

          <!-- Backdrop for closing menu -->
          <div v-if="showUserMenu" class="fixed inset-0 z-40" @click="closeUserMenu"></div>
        </div>
      </header>

      <!-- Page Content -->
      <main class="flex-1 overflow-y-auto p-8 relative">
        <slot></slot>
      </main>
    </div>

    <ChangePasswordModal v-if="showChangePasswordModal" @close="closeChangePasswordModal" />
  </div>
</template>
