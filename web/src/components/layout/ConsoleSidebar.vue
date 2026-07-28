<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'

import AppIcon from '@/components/ui/AppIcon.vue'
import { consoleNavItems, isNavItemActive } from '@/config/navigation'
import { useAuthStore } from '@/stores/auth'

const authStore = useAuthStore()
const route = useRoute()

const navItems = computed(() =>
  consoleNavItems.filter((item) => !item.requiresAdmin || authStore.isAdmin),
)
</script>

<template>
  <aside class="flex w-64 shrink-0 flex-col border-r border-stroke bg-surface-soft/30">
    <div class="relative flex h-16 items-center gap-2 border-b border-stroke/50 px-4 pr-11">
      <img src="/onlyboxes.avif" alt="" class="h-8 w-8 shrink-0 rounded-md object-cover" />
      <span class="text-lg font-bold tracking-tight">Onlyboxes</span>

      <div class="group absolute top-1/2 right-2 -translate-y-1/2">
        <button
          type="button"
          class="rounded-md p-1.5 text-tertiary"
          aria-label="Collapse sidebar (unavailable in the open-source edition)"
          disabled
        >
          <AppIcon name="chevron-left" :size="16" />
        </button>
        <span
          role="tooltip"
          class="pointer-events-none absolute top-[calc(100%+8px)] right-0 z-20 translate-y-1 rounded-default border border-stroke bg-surface px-2.5 py-1.5 text-xs whitespace-nowrap text-secondary opacity-0 shadow-card transition-[opacity,transform] duration-150 ease-out group-hover:translate-y-0 group-hover:opacity-100"
        >
          Unavailable in the open-source edition.
        </span>
      </div>
    </div>

    <nav class="flex-1 space-y-1 overflow-y-auto p-2.5" aria-label="Console sections">
      <RouterLink
        v-for="item in navItems"
        :key="item.to"
        :to="item.to"
        class="ui-focusable flex h-10 items-center gap-2 rounded-md px-3 text-sm font-medium transition-[background-color,color] duration-200"
        :class="
          isNavItemActive(route.path, item.to)
            ? 'bg-accent ui-on-accent'
            : 'text-secondary hover:bg-surface-soft hover:text-primary'
        "
        :aria-current="isNavItemActive(route.path, item.to) ? 'page' : undefined"
      >
        <AppIcon :name="item.icon" :size="18" />
        <span>{{ item.label }}</span>
      </RouterLink>
    </nav>

    <div
      class="flex items-center justify-between gap-2 border-t border-stroke/50 p-3.5 font-mono text-xs text-secondary"
    >
      <span class="truncate">Console {{ authStore.consoleVersion }}</span>
      <a
        :href="authStore.consoleRepoURL"
        target="_blank"
        rel="noopener noreferrer"
        :title="`Console ${authStore.consoleVersion} · GitHub`"
        aria-label="Onlyboxes Console GitHub"
        class="console-footer-link ui-focusable inline-flex shrink-0 items-center justify-center rounded-md p-1.5 text-secondary transition-colors hover:bg-surface-soft hover:text-primary"
      >
        <AppIcon name="github" :size="16" />
      </a>
    </div>
  </aside>
</template>
