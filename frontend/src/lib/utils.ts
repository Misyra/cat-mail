/**
 * 拼接 className（过滤假值），替代 clsx 的极简实现。
 * 对宽度类（w-*）做「后者胜出」合并：ui 组件内置 w-full，调用方传 w-36 等
 * 定宽类时必须覆盖默认值——Tailwind 生成的 CSS 源序固定，class 属性顺序不起作用。
 * 仅处理 w- 前缀（min-w-/max-w- 是不同属性，不参与合并）。
 */
export function cn(...classes: Array<string | false | null | undefined>): string {
  const parts = classes.filter(Boolean).join(' ').split(/\s+/)
  const widthIndexes: number[] = []
  for (let i = 0; i < parts.length; i++) {
    if (/^w-[\w.[\]#%()/]+$/.test(parts[i])) widthIndexes.push(i)
  }
  // 保留最后一个宽度类，移除其余（元素只能有一个 width）
  for (let j = widthIndexes.length - 2; j >= 0; j--) {
    parts[widthIndexes[j]] = ''
  }
  return parts.filter(Boolean).join(' ')
}

/** 解析整数字符串；空串或非法输入返回 undefined（表单数字字段通用） */
export function toInt(text: string): number | undefined {
  const trimmed = text.trim()
  if (!trimmed) return undefined
  const n = Number.parseInt(trimmed, 10)
  return Number.isFinite(n) ? n : undefined
}

/**
 * UTF-8 字节长度。后端密码上限 72 按 bcrypt 输入字节计（len(p)），
 * 与 JS 字符串 length（UTF-16 码元）不同——中文每字 3 字节，须按字节校验。
 */
export function utf8ByteLength(text: string): number {
  return new TextEncoder().encode(text).length
}
