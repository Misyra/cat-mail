import { Droplet, Monitor, Moon, Sun, type LucideIcon } from 'lucide-react'
import { useCallback, useEffect, useState, useSyncExternalStore } from 'react'

/** 主题四态：浅色 / 液态玻璃 / 深色 / 跟随系统（localStorage 'catmail-theme'）。
 * 玻璃是「材质档」而非明暗档：切换时在当前明暗底色上叠加玻璃材质，基色冻结在切换
 * 时刻（记入 catmail-theme-base），期间不随系统实时变化——与预览「在浅/深底色上叠
 * 玻璃材质（跟随当前明暗）」一致。 */
export type Theme = 'light' | 'glass' | 'dark' | 'system'

/** 四态主题的图标与显示名 i18n key（顶栏按钮 / ⌘K 命令面板共用；循环切换：浅色 → 玻璃 → 深色 → 跟随系统；
 * key 指向 common:theme.*，消费处 t(themeMeta.labelKey) 渲染） */
export const THEME_META = {
  light: { labelKey: 'common:theme.light', icon: Sun },
  glass: { labelKey: 'common:theme.glass', icon: Droplet },
  dark: { labelKey: 'common:theme.dark', icon: Moon },
  system: { labelKey: 'common:theme.system', icon: Monitor },
} as const satisfies Record<Theme, { labelKey: string; icon: LucideIcon }>

const THEME_KEY = 'catmail-theme'

/** 玻璃档恢复用：最近一次解析出的明暗基色（applyTheme 每次都会刷新）。
 * index.html 首屏内联脚本按同一键恢复，双保险一致 */
const THEME_BASE_KEY = 'catmail-theme-base'

const SYSTEM_DARK_QUERY = '(prefers-color-scheme: dark)'

function systemPrefersDark(): boolean {
  return typeof window.matchMedia === 'function' && window.matchMedia(SYSTEM_DARK_QUERY).matches
}

/** 读取持久化明暗基色；无记录或非法值时跟随系统（玻璃档首次恢复的兜底） */
function readStoredBase(): 'light' | 'dark' {
  try {
    const raw = localStorage.getItem(THEME_BASE_KEY)
    if (raw === 'light' || raw === 'dark') return raw
  } catch {
    // 隐私模式等读取失败时按默认处理
  }
  return systemPrefersDark() ? 'dark' : 'light'
}

/** 读取持久化主题；无记录或非法值时默认跟随系统 */
export function readStoredTheme(): Theme {
  try {
    const raw = localStorage.getItem(THEME_KEY)
    if (raw === 'light' || raw === 'glass' || raw === 'dark' || raw === 'system') return raw
  } catch {
    // 隐私模式等读取失败时按默认处理
  }
  return 'system'
}

/** 四态解析为实际生效的明暗（玻璃档 = 冻结的基色，不查系统实时偏好） */
function resolveTheme(theme: Theme): 'light' | 'dark' {
  if (theme === 'light' || theme === 'dark') return theme
  if (theme === 'glass') return readStoredBase()
  return systemPrefersDark() ? 'dark' : 'light'
}

/** 浅/深两态对应的 theme-color（移动端浏览器 UI 底色；与 index.html 首屏内联脚本的取值一致） */
const THEME_COLOR: Record<'light' | 'dark', string> = {
  light: '#f1f5f9',
  dark: '#020617',
}

/** 把主题落到 html.dark / html.glass class（Tailwind dark variant 与玻璃材质都依赖），
 * 并同步 theme-color meta；玻璃档顺带把当前基色记入 catmail-theme-base 供下次恢复
 * （启动/切换/storage 同步都经此函数） */
export function applyTheme(theme: Theme): void {
  const resolved = resolveTheme(theme)
  const root = document.documentElement
  root.classList.toggle('dark', resolved === 'dark')
  root.classList.toggle('glass', theme === 'glass')
  document.getElementById('theme-color-meta')?.setAttribute('content', THEME_COLOR[resolved])
  try {
    localStorage.setItem(THEME_BASE_KEY, resolved)
  } catch {
    // 隐私模式等写入失败时玻璃档下次恢复走「跟随系统」兜底
  }
}

/* ---------------------------------- 轻量外部 store（顶栏/设置页共享同一状态） ---------------------------------- */

type Listener = () => void

const listeners = new Set<Listener>()

let currentTheme: Theme = readStoredTheme()

function notify(): void {
  for (const listener of listeners) listener()
}

/** 写 localStorage + 应用 html.dark + 广播给所有 useTheme 实例 */
function commitTheme(theme: Theme): void {
  currentTheme = theme
  try {
    localStorage.setItem(THEME_KEY, theme)
  } catch {
    // 隐私模式等写入失败时仅本次会话生效
  }
  applyTheme(theme)
  notify()
}

// system 态下跟随系统实时切换 html.dark（单实例监听即可）
if (typeof window.matchMedia === 'function') {
  window.matchMedia(SYSTEM_DARK_QUERY).addEventListener('change', () => {
    if (currentTheme === 'system') applyTheme('system')
  })
  // 其他标签页修改主题时同步
  window.addEventListener('storage', (e) => {
    if (e.key !== THEME_KEY) return
    currentTheme = readStoredTheme()
    applyTheme(currentTheme)
    notify()
  })
}

function subscribeTheme(listener: Listener): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

/** 顶栏循环顺序：浅色 → 玻璃 → 深色 → 跟随系统 → 浅色 */
const NEXT_THEME: Record<Theme, Theme> = { light: 'glass', glass: 'dark', dark: 'system', system: 'light' }

/** 顶栏/设置页共用的主题 hook：同一 localStorage 键，任一处修改两处即时同步 */
export function useTheme() {
  const theme = useSyncExternalStore(
    subscribeTheme,
    () => currentTheme,
    () => 'system' as Theme,
  )

  const setTheme = useCallback((next: Theme) => {
    commitTheme(next)
  }, [])

  const cycleTheme = useCallback(() => {
    commitTheme(NEXT_THEME[currentTheme])
  }, [])

  return { theme, setTheme, cycleTheme }
}

/** 跟踪 html.dark 的即时变化（供 recharts / sonner 等非 Tailwind 区域适配深浅色） */
export function useIsDarkMode(): boolean {
  const [isDark, setIsDark] = useState(() => document.documentElement.classList.contains('dark'))
  useEffect(() => {
    const observer = new MutationObserver(() => {
      setIsDark(document.documentElement.classList.contains('dark'))
    })
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })
    return () => observer.disconnect()
  }, [])
  return isDark
}
