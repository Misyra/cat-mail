/**
 * i18n 词典校验（波次验收门禁）：
 * 1. zh-CN 与 en-US 每个命名空间的 key 结构必须同构（缺失/多余都报错）；
 * 2. zh-CN 叶子值必须非空；
 * 3. 命名空间文件对必须成对存在。
 * 用法：node scripts/check-i18n.mjs（退出码非 0 = 校验失败）
 */
import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

const LOCALES_DIR = new URL('../src/i18n/locales', import.meta.url).pathname

function readJsonSafe(path) {
  try {
    return JSON.parse(readFileSync(path, 'utf8'))
  } catch (err) {
    return { __parseError__: String(err) }
  }
}

/** 递归收集叶子 key 路径与空值 */
function collect(node, prefix = '', out = { leaves: new Set(), empties: [] }) {
  if (node === null || typeof node !== 'object') {
    out.leaves.add(prefix)
    if (node === '' || node === null) out.empties.push(prefix)
    return out
  }
  for (const [k, v] of Object.entries(node)) collect(v, prefix ? `${prefix}.${k}` : k, out)
  return out
}

const zhDir = join(LOCALES_DIR, 'zh-CN')
const enDir = join(LOCALES_DIR, 'en-US')
const zhFiles = readdirSync(zhDir).filter((f) => f.endsWith('.json')).sort()
const enFiles = readdirSync(enDir).filter((f) => f.endsWith('.json')).sort()

const errors = []

// 1. 文件对成对
for (const f of zhFiles) if (!enFiles.includes(f)) errors.push(`缺 en-US/${f}`)
for (const f of enFiles) if (!zhFiles.includes(f)) errors.push(`缺 zh-CN/${f}`)

let totalKeys = 0
for (const f of zhFiles) {
  if (!enFiles.includes(f)) continue
  const zh = readJsonSafe(join(zhDir, f))
  const en = readJsonSafe(join(enDir, f))
  if (zh.__parseError__) errors.push(`zh-CN/${f} JSON 解析失败: ${zh.__parseError__}`)
  if (en.__parseError__) errors.push(`en-US/${f} JSON 解析失败: ${en.__parseError__}`)
  if (zh.__parseError__ || en.__parseError__) continue

  const zhOut = collect(zh)
  const enOut = collect(en)
  totalKeys += zhOut.leaves.size

  // 复数 key 按去后缀归一化比较（zh 仅有 _other，en 合法多出 _one/_two 等）
  const stripPlural = (set) => new Set([...set].map((k) => k.replace(/_(one|two|few|many|zero|other)$/, '')))
  const zhKeys = stripPlural(zhOut.leaves)
  const enKeys = stripPlural(enOut.leaves)
  const diff1 = [...zhKeys].filter((k) => !enKeys.has(k))
  const diff2 = [...enKeys].filter((k) => !zhKeys.has(k))
  for (const k of diff1) errors.push(`en-US/${f} 缺 key: ${k}`)
  for (const k of diff2) errors.push(`en-US/${f} 多余 key: ${k}`)
  for (const k of zhOut.empties) errors.push(`zh-CN/${f} 空 value: ${k}`)
}

if (errors.length) {
  console.error(`✗ i18n 校验失败（${errors.length} 项）:`)
  for (const e of errors) console.error(`  - ${e}`)
  process.exit(1)
}
console.log(`✓ i18n 词典校验通过：${zhFiles.length} 个命名空间，共 ${totalKeys} 条 zh-CN key，zh/en 同构`)
