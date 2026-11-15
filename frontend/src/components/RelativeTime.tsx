import { useSyncExternalStore } from 'react'
import { useTranslation } from 'react-i18next'

import { formatDateTime, formatRelativeTime } from '@/lib/format'
import { cn } from '@/lib/utils'

/* --------------------------- O43 模块级共享时钟 --------------------------- */
/**
 * 所有 RelativeTime 实例共享同一个 30s 时钟，替代每实例各自的 setInterval
 * （Keys/Channels/Audit 页面峰值可挂 50–100 个实例，各自定时器浪费且行为不一）。
 * 快照用「30s 桶」整数（floor(now / 30s)）保证 getSnapshot 幂等（Date.now() 每次调用都不同，
 * 不能直接做快照）；tick 时同时记录桶号与当时的真实毫秒值，渲染用毫秒值精确计算相对文案，
 * 同一次 render 内两者配套且稳定。
 */

/** 30s 桶间隔（毫秒） */
const TICK_MS = 30_000

/** 当前快照：桶号 + tick 时刻的真实毫秒值（模块加载时初始化，之后仅随 tick 更新） */
const initialNow = Date.now()
let snapshotBucket = Math.floor(initialNow / TICK_MS)
let snapshotNowMs = initialNow

const listeners = new Set<() => void>()
let timer: number | null = null

/** tick：同时刷新桶号与真实毫秒快照（两者配套，保证同一次 render 内取值稳定） */
function tick(): void {
  snapshotNowMs = Date.now()
  snapshotBucket = Math.floor(snapshotNowMs / TICK_MS)
}

function startClock(): void {
  // 已在跑或页面隐藏时不开表（隐藏期间新订阅的场景，等恢复可见时再启动）
  if (timer !== null || document.hidden) return
  timer = window.setInterval(() => {
    tick()
    for (const notify of listeners) notify()
  }, TICK_MS)
}

function stopClock(): void {
  if (timer === null) return
  window.clearInterval(timer)
  timer = null
}

/** 页面隐藏时停表省电；恢复可见时立即 tick 一次（后台期间桶号多半已推进）并重启 */
function onVisibilityChange(): void {
  if (document.hidden) {
    stopClock()
  } else if (listeners.size > 0) {
    stopClock() // 防御：隐藏期间新订阅不会开表，这里保证从干净状态重启
    tick()
    for (const notify of listeners) notify()
    startClock()
  }
}

/** 订阅共享时钟（引用计数：第一个订阅者启动定时器并挂 visibilitychange，最后一个退订时清理） */
function subscribe(notify: () => void): () => void {
  if (listeners.size === 0) {
    // 首个订阅者先刷新一次快照：停表期间模块状态可能已过期，避免首帧用旧时间渲染
    tick()
    document.addEventListener('visibilitychange', onVisibilityChange)
    startClock()
  }
  listeners.add(notify)
  return () => {
    listeners.delete(notify)
    if (listeners.size === 0) {
      stopClock()
      document.removeEventListener('visibilitychange', onVisibilityChange)
    }
  }
}

/** getSnapshot：30s 桶号（tick 之间恒定，幂等） */
function getSnapshot(): number {
  return snapshotBucket
}

/**
 * 表格时间列统一展示（SPEC §8）：相对时间（如「3 分钟前」）+ hover 显绝对时间。
 * 通过 useSyncExternalStore 订阅模块级 30s 共享时钟，所有实例同批重渲染、整页仅 1 个定时器。
 * O30 语义：根元素为 <time dateTime>，供浏览器/读屏识别时间戳。
 */
export function RelativeTime({
  iso,
  className,
}: {
  iso: string | null | undefined
  className?: string
}) {
  useSyncExternalStore(subscribe, getSnapshot, () => 0)
  // 订阅语言切换触发重渲染（formatRelativeTime 在渲染期经 i18next 运行时求值，
  // 文案随语言变，本组件不直接调 t 但必须挂 hook 才能收到 languageChanged）
  useTranslation(['common'])
  // 与当前桶配套的真实毫秒快照（tick 时同步更新，单次同步渲染内只读一次、值稳定）
  const now = snapshotNowMs

  return (
    <time
      dateTime={iso ?? undefined}
      className={cn('whitespace-nowrap text-slate-500 dark:text-slate-400', className)}
      title={iso ? formatDateTime(iso, true) : undefined}
    >
      {formatRelativeTime(iso, now)}
    </time>
  )
}
