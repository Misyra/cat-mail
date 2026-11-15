import { useCallback, useSyncExternalStore } from 'react'

/** 响应式媒体查询 hook（如 '(min-width: 1024px)'）；SSR/无窗口环境返回 false */
export function useMediaQuery(query: string): boolean {
  // subscribe 需要稳定标识，否则每次渲染都会重挂监听；query 变化时才重建
  const subscribe = useCallback(
    (onChange: () => void) => {
      const mql = window.matchMedia(query)
      mql.addEventListener('change', onChange)
      return () => mql.removeEventListener('change', onChange)
    },
    [query],
  )
  return useSyncExternalStore(
    subscribe,
    () => window.matchMedia(query).matches,
    () => false,
  )
}
