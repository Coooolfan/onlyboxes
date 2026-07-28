import { createRouter, createWebHashHistory } from 'vue-router'

import { useViewTransitions } from '@/composables/useViewTransitions'
import { useAuthStore } from '@/stores/auth'

const LandingRouteView = { render: () => null }

const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    {
      path: '/',
      name: 'home',
      component: LandingRouteView,
      meta: {
        resolveLanding: true,
        layout: 'default',
      },
    },
    {
      path: '/login',
      name: 'login',
      component: () => import('@/views/LoginPage.vue'),
      meta: {
        layout: 'default',
      },
    },
    {
      path: '/workers',
      name: 'workers',
      component: () => import('@/views/WorkersPage.vue'),
      meta: {
        requiresAuth: true,
        layout: 'dashboard',
      },
    },
    {
      path: '/tools/worker-startup',
      name: 'worker-startup-tool',
      component: () => import('@/views/WorkerStartupToolPage.vue'),
      meta: {
        requiresAuth: true,
        layout: 'dashboard',
      },
    },
    {
      path: '/accounts',
      name: 'accounts',
      component: () => import('@/views/AccountsPage.vue'),
      meta: {
        requiresAuth: true,
        requiresAdmin: true,
        layout: 'dashboard',
      },
    },
    {
      path: '/tokens',
      name: 'tokens',
      component: () => import('@/views/TokensPage.vue'),
      meta: {
        requiresAuth: true,
        layout: 'dashboard',
      },
    },
    {
      path: '/:pathMatch(.*)*',
      name: 'not-found',
      component: LandingRouteView,
      meta: {
        resolveLanding: true,
        layout: 'default',
      },
    },
  ],
})

router.beforeEach(async (to) => {
  const authStore = useAuthStore()

  try {
    await authStore.bootstrap()
  } catch {
    // fall through with unauthenticated state and let pages render their own errors
  }

  if (to.meta.resolveLanding) {
    return authStore.isAuthenticated ? authStore.homePath : '/login'
  }

  if (to.meta.requiresAuth && !authStore.isAuthenticated) {
    return {
      path: '/login',
      query: {
        redirect: to.fullPath,
      },
    }
  }

  if (to.meta.requiresAdmin && !authStore.isAdmin) {
    return '/tokens'
  }

  if (to.path === '/login' && authStore.isAuthenticated) {
    const redirect =
      typeof to.query.redirect === 'string' && to.query.redirect.startsWith('/')
        ? to.query.redirect
        : authStore.homePath
    return redirect
  }

  return true
})

useViewTransitions(router)

export default router
