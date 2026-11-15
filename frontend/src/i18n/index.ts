import i18next, { type ResourceLanguage } from 'i18next'
import { useSyncExternalStore } from 'react'
import { initReactI18next } from 'react-i18next'

/**
 * i18n 入口（SPEC §国际化）：zh-CN 源语言 + en-US 翻译，react-i18next 实现。
 * 资源按领域拆分为命名空间文件（locales/<lang>/<ns>.json），import.meta.glob
 * 自动收编——新增命名空间只需建文件对，无需改本文件；TS key 校验见 i18next.d.ts。
 * 语言解析优先级：localStorage 'catmail-lang' → 浏览器语言（zh 前缀 / en 前缀）→ 默认 zh-CN。
 */

export type Language = 'zh-CN' | 'en-US'

const LANG_KEY = 'catmail-lang'

function detectBrowserLanguage(): Language {
  const candidates = navigator.languages?.length ? navigator.languages : [navigator.language]
  for (const lang of candidates) {
    const lower = (lang ?? '').toLowerCase()
    if (lower.startsWith('zh')) return 'zh-CN'
    if (lower.startsWith('en')) return 'en-US'
  }
  return 'zh-CN'
}

/** 读取持久化语言；无记录或非法值时按浏览器语言探测 */
export function readStoredLanguage(): Language {
  try {
    const raw = localStorage.getItem(LANG_KEY)
    if (raw === 'zh-CN' || raw === 'en-US') return raw
  } catch {
    // 隐私模式等读取失败时按浏览器语言兜底
  }
  return detectBrowserLanguage()
}

// glob 收编各命名空间 JSON（import: 'default' 直接取 JSON 对象本身）；
// glob 推导为 unknown，此处断言为 ResourceLanguage——key 级类型安全由 i18next.d.ts 以 zh-CN 词典推导
function pack(mods: Record<string, unknown>): ResourceLanguage {
  return Object.fromEntries(
    Object.entries(mods).map(([path, mod]) => [path.split('/').pop()!.replace(/\.json$/, ''), mod]),
  ) as ResourceLanguage
}

const zhCN = pack(import.meta.glob('./locales/zh-CN/*.json', { eager: true, import: 'default' }))
const enUS = pack(import.meta.glob('./locales/en-US/*.json', { eager: true, import: 'default' }))

if (!i18next.isInitialized) {
  void i18next.use(initReactI18next).init({
    resources: { 'zh-CN': zhCN, 'en-US': enUS },
    lng: readStoredLanguage(),
    fallbackLng: 'zh-CN',
    returnEmptyString: false,
    // React 自身转义文本，插值无需 HTML 转义
    interpolation: { escapeValue: false },
  })
}

// <html lang> 跟随当前语言（a11y 与字体渲染）
document.documentElement.lang = i18next.language

/* ---------------- 轻量外部 store（语言切换器共享同一状态，模式同 lib/theme.ts） ---------------- */

type Listener = () => void

const listeners = new Set<Listener>()

i18next.on('languageChanged', (lng) => {
  document.documentElement.lang = lng
  for (const listener of listeners) listener()
})

function subscribeLanguage(listener: Listener): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

/** 写 localStorage + 切换 i18next 语言（所有 useTranslation/useLanguage 实例即时重渲染） */
export function setLanguage(lng: Language): void {
  void i18next.changeLanguage(lng)
  try {
    localStorage.setItem(LANG_KEY, lng)
  } catch {
    // 隐私模式等写入失败时仅本次会话生效
  }
}

/** 当前语言（响应式：切换语言后重渲染） */
export function useLanguage(): Language {
  return useSyncExternalStore(
    subscribeLanguage,
    () => i18next.language as Language,
    () => 'zh-CN' as Language,
  )
}
