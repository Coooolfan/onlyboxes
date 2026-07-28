<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'

import AppButton from '@/components/ui/AppButton.vue'
import { lockBodyScroll } from '@/composables/useBodyScrollLock'

type ModalSize = 'sm' | 'md' | 'lg' | 'xl'

const props = withDefaults(
  defineProps<{
    open?: boolean
    title: string
    description?: string
    size?: ModalSize
    /** Extra classes applied to the dialog panel. */
    panelClass?: string
    /** Disables overlay click, escape key and the close button. */
    persistent?: boolean
    /** Hides the header close button while keeping escape / overlay dismissal. */
    hideCloseButton?: boolean
  }>(),
  {
    open: true,
    size: 'md',
    persistent: false,
    hideCloseButton: false,
  },
)

const emit = defineEmits<{ close: [] }>()

const widthClass: Record<ModalSize, string> = {
  sm: 'w-[min(440px,100%)]',
  md: 'w-[min(560px,100%)]',
  lg: 'w-[min(640px,100%)]',
  xl: 'w-[min(720px,100%)]',
}

const panelRef = ref<HTMLElement | null>(null)
const titleId = `modal-title-${Math.random().toString(36).slice(2, 9)}`
const descriptionId = computed(() => (props.description ? `${titleId}-desc` : undefined))

let releaseScrollLock: (() => void) | null = null

function requestClose(): void {
  if (props.persistent) {
    return
  }
  emit('close')
}

function handleEscape(event: KeyboardEvent): void {
  if (event.key !== 'Escape') {
    return
  }
  event.stopPropagation()
  requestClose()
}

function handleTabTrap(event: KeyboardEvent): void {
  if (event.key !== 'Tab') {
    return
  }

  const panel = panelRef.value
  if (!panel) {
    return
  }

  const focusable = Array.from(
    panel.querySelectorAll<HTMLElement>(
      'a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])',
    ),
  ).filter((node) => node.offsetParent !== null || node === document.activeElement)

  if (focusable.length === 0) {
    return
  }

  const first = focusable[0]!
  const last = focusable[focusable.length - 1]!
  const active = document.activeElement

  if (event.shiftKey && (active === first || !panel.contains(active))) {
    event.preventDefault()
    last.focus()
    return
  }
  if (!event.shiftKey && active === last) {
    event.preventDefault()
    first.focus()
  }
}

function focusInitialElement(): void {
  const panel = panelRef.value
  if (!panel) {
    return
  }
  const target =
    panel.querySelector<HTMLElement>('[data-autofocus]') ??
    panel.querySelector<HTMLElement>(
      'input:not([disabled]), textarea:not([disabled]), button:not([disabled])',
    )
  target?.focus()
}

watch(
  () => props.open,
  async (open) => {
    if (open) {
      releaseScrollLock ??= lockBodyScroll()
      document.addEventListener('keydown', handleEscape)
      await nextTick()
      focusInitialElement()
      return
    }
    teardown()
  },
  { immediate: true },
)

function teardown(): void {
  releaseScrollLock?.()
  releaseScrollLock = null
  if (typeof document !== 'undefined') {
    document.removeEventListener('keydown', handleEscape)
  }
}

onBeforeUnmount(teardown)
</script>

<template>
  <Teleport to="body">
    <Transition name="modal">
      <div
        v-if="open"
        class="ui-modal-overlay fixed inset-0 z-50 flex items-center justify-center overflow-y-auto p-6"
        @click.self="requestClose"
        @keydown="handleTabTrap"
      >
        <div
          ref="panelRef"
          class="ui-modal-panel my-auto flex max-h-full flex-col rounded-lg border border-stroke bg-surface shadow-modal"
          :class="[widthClass[size], panelClass]"
          role="dialog"
          aria-modal="true"
          :aria-labelledby="titleId"
          :aria-describedby="descriptionId"
          tabindex="-1"
        >
          <header class="flex items-start justify-between gap-4 border-b border-stroke px-6 py-5">
            <div class="grid gap-1">
              <h2 :id="titleId" class="m-0 text-xl font-semibold">{{ title }}</h2>
              <p v-if="description" :id="descriptionId" class="m-0 text-sm text-secondary">
                {{ description }}
              </p>
            </div>
            <AppButton
              v-if="!hideCloseButton && !persistent"
              variant="ghost"
              size="sm"
              icon="close"
              icon-only
              aria-label="Close dialog"
              class="-mt-1 -mr-1.5"
              @click="requestClose"
            />
          </header>

          <div class="grid gap-5 overflow-y-auto p-6">
            <slot />
          </div>

          <footer
            v-if="$slots.footer"
            class="flex justify-end gap-3 rounded-b-lg border-t border-stroke px-6 py-5 max-[600px]:flex-col-reverse max-[600px]:[&>*]:w-full"
          >
            <slot name="footer" />
          </footer>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>
