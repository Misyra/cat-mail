/**
 * 轻量 SVG 迷你趋势图（Grafana stat panel 风格）：折线 + 低透明度面积。
 * 手绘 polyline/area path，不引 recharts 单卡实例，省渲染开销；
 * 有效点少于 2 个（含无数据）时返回 null 隐藏不占位。
 */

const VIEW_W = 100
const VIEW_H = 36
const PAD = 3

/** 连续非空段：null（无数据日）处断线，避免把缺口两侧强行连线 */
interface Segment {
  points: Array<{ x: number; y: number }>
}

export interface SparklineProps {
  /** 序列值；null = 无数据点（断线） */
  data: Array<number | null>
  /** 线与面积主色（hex，语义四色由调用方按趋势选定） */
  color: string
  className?: string
}

/** 值域映射到 viewBox 坐标（等比拉伸由 preserveAspectRatio="none" 完成） */
function buildSegments(data: Array<number | null>): Segment[] {
  const valid = data.filter((v): v is number => v !== null)
  if (valid.length < 2) return []
  const min = Math.min(...valid)
  const max = Math.max(...valid)
  const span = max - min
  const n = data.length
  const xOf = (i: number): number => PAD + (i / (n - 1)) * (VIEW_W - 2 * PAD)
  const yOf = (v: number): number => {
    // 常数序列画在中线，不贴底
    if (span === 0) return VIEW_H / 2
    return VIEW_H - PAD - ((v - min) / span) * (VIEW_H - 2 * PAD)
  }

  const segments: Segment[] = []
  let current: Segment | null = null
  data.forEach((v, i) => {
    if (v === null) {
      current = null
      return
    }
    if (!current) {
      current = { points: [] }
      segments.push(current)
    }
    current.points.push({ x: xOf(i), y: yOf(v) })
  })
  return segments
}

/** 折线 path（M/L 直连，不插值；两端圆头） */
function linePath(points: Segment['points']): string {
  return points
    .map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x.toFixed(2)} ${p.y.toFixed(2)}`)
    .join(' ')
}

/** 面积 path：折线闭合到底边（低透明度填充，无描边） */
function areaPath(points: Segment['points']): string {
  const first = points[0]
  const last = points[points.length - 1]
  const bottom = VIEW_H - PAD / 2
  return `${linePath(points)} L${last.x.toFixed(2)} ${bottom} L${first.x.toFixed(2)} ${bottom} Z`
}

export function Sparkline({ data, color, className }: SparklineProps) {
  const segments = buildSegments(data)
  if (segments.length === 0) return null

  return (
    <svg
      viewBox={`0 0 ${VIEW_W} ${VIEW_H}`}
      preserveAspectRatio="none"
      className={className}
      aria-hidden
      focusable="false"
    >
      {segments.map((seg, i) => (
        <path
          key={`a${i}`}
          d={areaPath(seg.points)}
          fill={color}
          fillOpacity={0.12}
          stroke="none"
        />
      ))}
      {segments.map((seg, i) => (
        <path
          key={`l${i}`}
          d={linePath(seg.points)}
          fill="none"
          stroke={color}
          strokeWidth={1.5}
          strokeLinecap="round"
          strokeLinejoin="round"
          // viewBox 非等比拉伸会导致描边变形：锁定物理像素宽度
          vectorEffect="non-scaling-stroke"
        />
      ))}
    </svg>
  )
}
