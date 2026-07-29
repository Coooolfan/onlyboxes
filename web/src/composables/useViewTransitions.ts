import { nextTick } from 'vue'
import type { Router } from 'vue-router'

/**
 * The View Transitions API is still missing from some engines, and the typings
 * ship separately from `lib.dom`, so narrow it locally instead of widening the
 * global `Document` type.
 */
type ViewTransitionDocument = Document & {
  startViewTransition?: (callback: () => Promise<void> | void) => { finished: Promise<void> }
}

function supportsViewTransitions(): boolean {
  if (typeof document === 'undefined') {
    return false
  }
  if (typeof (document as ViewTransitionDocument).startViewTransition !== 'function') {
    return false
  }
  // Honour the OS-level motion preference: a cross-fade plus a sliding highlight
  // is exactly the kind of motion this setting asks us to drop.
  return !window.matchMedia('(prefers-reduced-motion: reduce)').matches
}

/**
 * Routes navigations through `document.startViewTransition` so the browser can
 * tween between the two page states.
 *
 * The API wants a callback that mutates the DOM and resolves once the new state
 * is painted, but Vue Router drives that mutation itself. So the callback
 * releases the pending navigation, then parks on a promise that `afterEach`
 * settles on the next tick — at which point the incoming view has rendered and
 * the browser can take its second snapshot.
 */
export function useViewTransitions(router: Router): void {
  let finishTransition: (() => void) | null = null

  router.beforeResolve((to, from) => {
    // A first paint has nothing to transition from, and replacing only the query
    // or hash leaves the view in place.
    if (from.matched.length === 0 || to.path === from.path) {
      return
    }
    if (!supportsViewTransitions()) {
      return
    }

    // From the first real navigation onwards the entrance is the transition's
    // job. Two reasons to hand it over rather than toggle it per navigation:
    // the incoming snapshot is taken exactly when `.ui-rise` sits on its first
    // keyframe at `opacity: 0`, so leaving it on cross-fades into a blank page;
    // and restoring it afterwards simply restarts the animation, replaying the
    // rise on top of the transition that just finished.
    //
    // The class is never removed. The initial page load keeps `.ui-rise`
    // because no navigation has happened yet, which is the only place it still
    // earns its keep.
    document.documentElement.classList.add('vt-owns-entrance')

    // The sidebar item animates its own background over 200ms when it stops
    // being active. That tween is still near its starting colour when the new
    // snapshot is taken, which freezes a dark background into the item being
    // left — it then reappears the moment the transition ends and the live DOM
    // takes over. Freezing those tweens for the duration hands the hand-off
    // entirely to the transition.
    document.documentElement.classList.add('vt-running')

    return new Promise<void>((releaseNavigation) => {
      const transition = (document as ViewTransitionDocument).startViewTransition!(() => {
        releaseNavigation()
        return new Promise<void>((done) => {
          finishTransition = done
        })
      })

      transition.finished.finally(() => {
        document.documentElement.classList.remove('vt-running')
      })
    })
  })

  router.afterEach(async () => {
    if (!finishTransition) {
      return
    }
    await nextTick()
    finishTransition()
    finishTransition = null
  })

  // A cancelled navigation never reaches afterEach, which would otherwise leave
  // the transition pending and the page frozen on its old snapshot.
  router.onError(() => {
    finishTransition?.()
    finishTransition = null
    document.documentElement.classList.remove('vt-running')
  })
}
