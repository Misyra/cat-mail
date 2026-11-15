/**
 * 剪贴板写入统一封装（O38）：优先异步 Clipboard API（仅 https/localhost 等安全源可用）；
 * 自托管常见的 http://LAN 非安全源下 API 不存在或写入被拒时，降级为
 * 临时 textarea + document.execCommand('copy')。两条路径都失败才抛错，由调用方统一提示。
 */

/** 降级路径：插入临时 textarea 选中后模拟复制，finally 保证节点移除 */
function copyViaExecCommand(text: string): void {
  const textarea = document.createElement('textarea')
  textarea.value = text
  // fixed + 全透明：避免插入过程闪现或引起页面滚动
  textarea.style.position = 'fixed'
  textarea.style.opacity = '0'
  document.body.appendChild(textarea)
  try {
    textarea.select()
    if (!document.execCommand('copy')) throw new Error('copy failed')
  } finally {
    document.body.removeChild(textarea)
  }
}

/** 复制文本到剪贴板；成功 resolve，原生 API 与 execCommand 均不可用时 reject */
export async function copyText(text: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return
    } catch {
      // 非安全源 / 权限被拒等 → 继续走 execCommand 降级
    }
  }
  copyViaExecCommand(text)
}
