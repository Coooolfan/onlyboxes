import { createRouter, createWebHashHistory } from 'vue-router'

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
      },
    },
    {
      path: '/login',
      name: 'login',
      component: () => import('@/views/LoginPage.vue'),
    },
    {
      path: '/workers',
      name: 'workers',
      component: () => import('@/views/WorkersPage.vue'),
      meta: {
        requiresAuth: true,
        requiresAdmin: true,
      },
    },
    {
      path: '/tokens',
      name: 'tokens',
      component: () => import('@/views/TokensPage.vue'),
      meta: {
        requiresAuth: true,
      },
    },
    {
      path: '/:pathMatch(.*)*',
      name: 'not-found',
      component: LandingRouteView,
      meta: {
        resolveLanding: true,
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

export default router
