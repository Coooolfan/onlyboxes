import type { IconName } from '@/components/ui/icons'

export interface NavItem {
  label: string
  to: string
  icon: IconName
  requiresAdmin?: boolean
}

export const consoleNavItems: readonly NavItem[] = [
  { label: 'Workers', to: '/workers', icon: 'box' },
  { label: 'Accounts', to: '/accounts', icon: 'users', requiresAdmin: true },
  { label: 'Tokens', to: '/tokens', icon: 'key' },
  { label: 'Worker Tool', to: '/tools/worker-startup', icon: 'sliders' },
]

export function isNavItemActive(currentPath: string, itemPath: string): boolean {
  return currentPath === itemPath || currentPath.startsWith(`${itemPath}/`)
}
