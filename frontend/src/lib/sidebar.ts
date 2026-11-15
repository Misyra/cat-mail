import { useCallback, useSyncExternalStore } from 'react'

/** 侧栏折叠态：localStorage 'catmail-sidebar-collapsed'（'1' = 折叠），跨标签页 storage 事件同步 */
const SIDEBAR_KEY = 'catmail-sidebar-collapsed'

function readStoredCollapsed(): boolean {
  try {
    return localStorage.getItem(SIDEBAR_KEY) === '1'
  } catch {
    // 隐私模式等读取失败时按展开处理
    return false
  }
}

/* ---------------------------------- 轻量外部 store（写法同 theme.ts） ---------------------------------- */

type Listener = () => void

const listeners = new Set<Listener>()

let currentCollapsed = readStoredCollapsed()

function notify(): void {
  for (const listener of listeners) listener()
}

/** 写 localStorage + 广播给所有 useSidebarCollapsed 实例 */
function commitCollapsed(collapsed: boolean): void {
  currentCollapsed = collapsed
  try {
    localStorage.setItem(SIDEBAR_KEY, collapsed ? '1' : '0')
  } catch {
    // 隐私模式等写入失败时仅本次会话生效
  }
  notify()
}

// 其他标签页切换折叠态时同步（storage 事件只在非写入方标签页触发）
if (typeof window !== 'undefined') {
  window.addEventListener('storage', (e) => {
    if (e.key !== SIDEBAR_KEY) return
    currentCollapsed = readStoredCollapsed()
    notify()
  })
}

function subscribeCollapsed(listener: Listener): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

/** 侧栏折叠态 hook：同一 localStorage 键，所有标签页/实例即时同步 */
export function useSidebarCollapsed() {
  const collapsed = useSyncExternalStore(
    subscribeCollapsed,
    () => currentCollapsed,
    () => false,
  )

  const toggleCollapsed = useCallback(() => {
    commitCollapsed(!currentCollapsed)
  }, [])

  return { collapsed, toggleCollapsed }
}
