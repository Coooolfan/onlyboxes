<script setup lang="ts">
defineProps<{
  creatingWorker: boolean
  autoRefreshEnabled: boolean
  loading: boolean
  showCreateAccount?: boolean
}>()

const emit = defineEmits<{
  addWorker: []
  toggleAutoRefresh: []
  refresh: []
  logout: []
  createAccount: []
}>()
</script>

<template>
  <header class="flex items-start justify-between gap-5 bg-surface border border-stroke rounded-lg p-8 shadow-card animate-rise-in max-[960px]:flex-col">
    <div>
      <p class="m-0 font-mono text-xs tracking-[0.05em] uppercase text-secondary">Onlyboxes / Worker Registry</p>
      <h1 class="mt-3 mb-2 text-2xl font-semibold leading-[1.2] tracking-[-0.02em]">Execution Node Control Panel</h1>
      <p class="m-0 text-secondary text-sm max-w-[62ch] leading-normal">Real-time monitoring for worker registration and heartbeat health.</p>
    </div>

    <div class="flex items-center gap-3 max-[960px]:w-full max-[960px]:flex-wrap">
      <button
        v-if="showCreateAccount"
        class="rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
        type="button"
        @click="emit('createAccount')"
      >
        Create Account
      </button>
      <button
        class="rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center text-white bg-accent border border-accent transition-all duration-200 hover:not-disabled:bg-[#333] hover:not-disabled:border-[#333] disabled:cursor-not-allowed disabled:opacity-50"
        type="button"
        :disabled="creatingWorker"
        @click="emit('addWorker')"
      >
        {{ creatingWorker ? 'Adding...' : 'Add Worker' }}
      </button>
      <button
        class="rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
        type="button"
        @click="emit('toggleAutoRefresh')"
      >
        {{ autoRefreshEnabled ? 'Auto Refresh: ON' : 'Auto Refresh: OFF' }}
      </button>
      <button
        class="rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center text-white bg-accent border border-accent transition-all duration-200 hover:not-disabled:bg-[#333] hover:not-disabled:border-[#333] disabled:cursor-not-allowed disabled:opacity-50"
        type="button"
        :disabled="loading"
        @click="emit('refresh')"
      >
        {{ loading ? 'Refreshing...' : 'Refresh Now' }}
      </button>
      <button
        class="rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
        type="button"
        @click="emit('logout')"
      >
        Logout
      </button>
    </div>
  </header>
</template>
