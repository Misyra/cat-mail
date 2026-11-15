import {
  Activity,
  Ban,
  Globe2,
  History,
  KeyRound,
  LayoutDashboard,
  LayoutTemplate,
  PlugZap,
  ScrollText,
  Settings,
  Users,
  type LucideIcon,
} from 'lucide-react'

/** 导航项 label 存 i18n key（layout 命名空间、带 ns 前缀），由 Sidebar / AppLayout / CommandPalette 经 t() 渲染 */
export type NavLabelKey =
  | 'layout:nav.dashboard'
  | 'layout:nav.channels'
  | 'layout:nav.domains'
  | 'layout:nav.templates'
  | 'layout:nav.logs'
  | 'layout:nav.events'
  | 'layout:nav.suppressions'
  | 'layout:nav.apiKeys'
  | 'layout:nav.users'
  | 'layout:nav.audit'
  | 'layout:nav.settings'

export interface NavItem {
  to: string
  label: NavLabelKey
  icon: LucideIcon
  /** '/' 需要精确匹配，否则永远高亮 */
  end?: boolean
}

/** 侧栏导航（SPEC §6）：Sidebar 与顶栏标题共用此单一来源 */
export const NAV_ITEMS: NavItem[] = [
  { to: '/', label: 'layout:nav.dashboard', icon: LayoutDashboard, end: true },
  { to: '/channels', label: 'layout:nav.channels', icon: PlugZap },
  // 域名治理：与渠道管理同源（allowed_domains 聚合），紧随其后
  { to: '/domains', label: 'layout:nav.domains', icon: Globe2 },
  // 邮件模板：发送内容的复用骨架，紧邻发送前置资源、先于日志
  { to: '/templates', label: 'layout:nav.templates', icon: LayoutTemplate },
  { to: '/logs', label: 'layout:nav.logs', icon: ScrollText },
  // 送达链路三件套：送达事件查询与收件人抑制列表（紧随发送日志，同属投递观测）
  { to: '/events', label: 'layout:nav.events', icon: Activity },
  { to: '/suppressions', label: 'layout:nav.suppressions', icon: Ban },
  { to: '/keys', label: 'layout:nav.apiKeys', icon: KeyRound },
  { to: '/users', label: 'layout:nav.users', icon: Users },
  // E4 管理操作审计：与账号/设置同属管理类，置于两者之间
  { to: '/audit', label: 'layout:nav.audit', icon: History },
  { to: '/settings', label: 'layout:nav.settings', icon: Settings },
]
