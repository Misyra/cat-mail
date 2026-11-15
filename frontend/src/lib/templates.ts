import { keepPreviousData, useQuery, type QueryClient } from '@tanstack/react-query'

import { api } from '@/lib/api'

/**
 * 邮件模板数据层（GET/POST /api/templates、PUT/DELETE /api/templates/:id，
 * snake_case 原样字段名）。占位符渲染与变量提取的客户端实现与后端 render 包
 * 同口径：{{var}} 占位符、点路径取嵌套对象、HTML 语境转义、缺失变量留空。
 */

/** GET /api/templates 列表项（不含正文大字段，正文走详情接口） */
export interface TemplateItem {
  id: number
  name: string
  subject: string
  /** 模板变量清单（创建/更新未显式提供时由后端按内容自动提取） */
  variables: string[] | null
  created_at: string
  updated_at: string
}

/** GET /api/templates/:id 完整详情（含正文） */
export interface TemplateDetail extends TemplateItem {
  html_body: string
  text_body: string
}

export interface TemplateListResponse {
  items: TemplateItem[]
}

/** 模板创建/编辑请求体（variables 显式提供时以清单为准，缺省由后端自动提取） */
export interface TemplateInput {
  name: string
  subject: string
  html: string
  text: string
  variables?: string[]
}

export const TEMPLATES_QUERY_KEY = ['templates', 'list'] as const

/** GET /api/templates（列表，placeholderData 保持翻页/刷新稳定） */
export function useTemplates() {
  return useQuery({
    queryKey: TEMPLATES_QUERY_KEY,
    queryFn: () => api<TemplateListResponse>('/api/templates'),
    placeholderData: keepPreviousData,
  })
}

/** 模板写操作成功后失效列表缓存 */
export function invalidateTemplates(queryClient: QueryClient): void {
  void queryClient.invalidateQueries({ queryKey: ['templates'] })
}

/* ---------------------- 占位符渲染（与后端 render 包同口径） ---------------------- */

/** {{var}} 占位符：变量名为花括号间任意非空内容（去两侧空白），支持点路径 */
export const TEMPLATE_VAR_RE = /\{\{\s*([^{}]+?)\s*\}\}/g

/** 提取模板串中的占位符变量名（跨多个模板串合并后去重保序） */
export function extractTemplateVars(...tpls: string[]): string[] {
  const seen = new Set<string>()
  const out: string[] = []
  for (const tpl of tpls) {
    for (const m of tpl.matchAll(TEMPLATE_VAR_RE)) {
      const name = m[1].trim()
      if (!name || seen.has(name)) continue
      seen.add(name)
      out.push(name)
    }
  }
  return out
}

/** 按点路径取嵌套变量值（JSON 对象形态）；任一段缺失或值为 null → undefined */
function lookupVar(vars: Record<string, unknown>, path: string): unknown {
  let cur: unknown = vars
  for (const seg of path.split('.')) {
    if (cur === null || typeof cur !== 'object' || !(seg in (cur as Record<string, unknown>))) {
      return undefined
    }
    cur = (cur as Record<string, unknown>)[seg]
  }
  return cur === null ? undefined : cur
}

/** 变量值 → 字符串：字符串原样；数字/布尔/对象按 JSON 最小表示 */
function stringifyVar(v: unknown): string {
  if (typeof v === 'string') return v
  if (v === undefined) return ''
  try {
    return JSON.stringify(v) ?? ''
  } catch {
    return ''
  }
}

/** HTML 转义（变量注入防护，与后端 html.EscapeString 同义） */
function escapeHtml(s: string): string {
  return s
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&#34;')
    .replaceAll("'", '&#39;')
}

/**
 * 渲染单个模板串：未提供的变量占位符留空；escape=true 用于 HTML 语境（变量值
 * 转义），false 用于主题/纯文本语境（原样替换）。
 */
export function renderTemplate(tpl: string, vars: Record<string, unknown>, escape: boolean): string {
  if (!tpl.includes('{{')) return tpl
  return tpl.replace(TEMPLATE_VAR_RE, (_m, rawName: string) => {
    const name = rawName.trim()
    if (!name) return _m
    const v = lookupVar(vars, name)
    if (v === undefined) return ''
    const s = stringifyVar(v)
    return escape ? escapeHtml(s) : s
  })
}

/** 整段 HTML 转义（预览 text 正文包 <pre> 用） */
export function escapeTemplateHtml(s: string): string {
  return escapeHtml(s)
}

/** 解析样本变量 JSON（空串/空白 = {}）；失败返回 null 由调用方提示 */
export function parseSampleVars(raw: string): Record<string, unknown> | null {
  const trimmed = raw.trim()
  if (!trimmed) return {}
  try {
    const parsed: unknown = JSON.parse(trimmed)
    if (parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed)) {
      return parsed as Record<string, unknown>
    }
    return null
  } catch {
    return null
  }
}

/**
 * 构造预览 iframe 的完整 HTML 文档（sandbox 无脚本环境内渲染）：
 * 有 html_body 时按 HTML 语境渲染（变量转义）；否则按纯文本语境渲染 text_body
 * 并以 <pre> 展示。
 */
export function buildPreviewDoc(detail: { html_body: string; text_body: string }, vars: Record<string, unknown>): string {
  const style =
    'body{margin:0;padding:16px;color:#0f172a;background:#fff;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;font-size:14px;line-height:1.6;}'
  if (detail.html_body.trim()) {
    return (
      '<!doctype html><html><head><meta charset="utf-8">' +
      '<meta name="viewport" content="width=device-width,initial-scale=1">' +
      `<style>${style}</style></head><body>${renderTemplate(detail.html_body, vars, true)}</body></html>`
    )
  }
  const text = renderTemplate(detail.text_body, vars, false)
  return (
    '<!doctype html><html><head><meta charset="utf-8">' +
    '<meta name="viewport" content="width=device-width,initial-scale=1">' +
    `<style>${style}pre{margin:0;white-space:pre-wrap;word-break:break-word;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;}</style>` +
    `</head><body><pre>${escapeTemplateHtml(text)}</pre></body></html>`
  )
}
