<script setup lang="ts">
import { computed } from 'vue'

import AppIcon from '@/components/ui/AppIcon.vue'
import { defaultConsoleRepoURL, defaultConsoleVersion } from '@/constants/console'
import { useAuthStore } from '@/stores/auth'

const authStore = useAuthStore()

const consoleVersionText = computed(() => authStore.consoleVersion || defaultConsoleVersion)
const consoleRepoURL = computed(() => authStore.consoleRepoURL || defaultConsoleRepoURL)
</script>

<template>
  <div class="relative flex min-h-screen flex-col gap-4 px-6 pt-8 pb-5 max-[620px]:px-4">
    <div class="flex flex-1 items-center justify-center">
      <slot />
    </div>

    <footer
      class="mx-auto flex w-[min(1240px,100%)] items-center justify-end gap-2 font-mono text-xs leading-normal text-secondary max-[620px]:justify-start"
    >
      <span>Console {{ consoleVersionText }}</span>
      <span aria-hidden="true">·</span>
      <a
        class="console-footer-link ui-focusable inline-flex items-center gap-1.5 rounded-default px-1 py-0.5 text-secondary underline underline-offset-2 transition-colors hover:text-primary"
        :href="consoleRepoURL"
        target="_blank"
        rel="noopener noreferrer"
      >
        <AppIcon name="github" :size="13" />
        GitHub
      </a>
    </footer>
  </div>
</template>
