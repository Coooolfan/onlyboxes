<script setup lang="ts">
import { computed } from 'vue'
import { RouterView } from 'vue-router'

import { defaultConsoleRepoURL, defaultConsoleVersion } from '@/constants/console'
import { useAuthStore } from '@/stores/auth'

const authStore = useAuthStore()

const consoleVersionText = computed(() => authStore.consoleVersion || defaultConsoleVersion)
const consoleRepoURL = computed(() => authStore.consoleRepoURL || defaultConsoleRepoURL)
</script>

<template>
  <div class="relative min-h-screen px-6 pt-8 pb-5 flex flex-col gap-4 max-[620px]:px-4 max-[620px]:pt-6 max-[620px]:pb-4">
    <div class="flex-1">
      <RouterView />
    </div>
    <footer class="mx-auto w-[min(1240px,100%)] flex items-center justify-end gap-2 text-secondary text-xs leading-normal font-mono max-[620px]:justify-start">
      <span>Console {{ consoleVersionText }}</span>
      <span>·</span>
      <a
        class="text-secondary underline underline-offset-2 hover:text-primary"
        :href="consoleRepoURL"
        target="_blank"
        rel="noopener noreferrer"
      >
        GitHub
      </a>
    </footer>
  </div>
</template>
