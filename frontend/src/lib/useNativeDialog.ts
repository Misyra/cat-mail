/**
 * 原生 <dialog> 受控开关（W2 O28.1，Modal / ⌘K 命令面板共用）：
 * React 的 open 状态单向驱动 showModal()/close()；Esc（cancel）、点击外部（closedby="any"，
 * Safari 走坐标兜底）等关闭途径全部汇回 onClose，由状态翻转后再经 effect 驱动 dialog.close()。
 * 不手写 focus trap / role="dialog" / aria-modal / inert / 滚动锁——原生 modal 已提供
 * top layer、背景 inert、焦点圈定，close() 时浏览器自动把焦点归还给打开前的元素。
 */
import { useEffect, useRef, type RefObject } from 'react'

/** closedby 属性（点击外部关闭）是否原生可用；Safari 尚未支持，需要指针坐标兜底 */
const SUPPORTS_CLOSEDBY = 'closedBy' in HTMLDialogElement.prototype

export function useNativeDialog(
  open: boolean,
  onClose: () => void,
): RefObject<HTMLDialogElement | null> {
  const dialogRef = useRef<HTMLDialogElement | null>(null)
  // onClose 经 ref 转发，effect 只依赖 open（父组件内联箭头函数不会导致反复解绑/重开弹窗）
  const onCloseRef = useRef(onClose)
  useEffect(() => {
    onCloseRef.current = onClose
  }, [onClose])

  useEffect(() => {
    const dialog = dialogRef.current
    if (!dialog) return

    // 点击外部关闭：原生 closedby="any"。用 setAttribute 而非 JSX 属性（React 类型暂未收录该新属性）
    dialog.setAttribute('closedby', 'any')

    const onCancel = (event: Event) => {
      // 受控模式：拦截浏览器自行 close（Esc / closedby 点击外部都会发 cancel），统一汇回 onClose，
      // 由 React 状态翻转后经本 effect 的 close() 收尾
      event.preventDefault()
      onCloseRef.current()
    }
    const onCloseEvent = () => {
      // close 事件 → 同步回 React 状态（父组件重复 setOpen(false) 无害）。
      // dialog.open 仍为 true 时说明是 StrictMode 下「cleanup close → 重新 showModal」留下的
      // 过期事件（close 事件异步派发，此时弹窗已被 effect 重新打开），状态未变，跳过
      if (!dialog.open) onCloseRef.current()
    }
    dialog.addEventListener('cancel', onCancel)
    dialog.addEventListener('close', onCloseEvent)

    // Safari 无 closedby：按下与抬起都在 dialog 矩形之外才关闭。监听挂在 dialog 上——惰性背景上的
    // 点击会被浏览器重定向到 modal dialog 本身，事件仍能到达，clientX/Y 是物理坐标。
    // 用坐标判定而非 event.target：dialog 自身 padding 与子元素间隙上 target 也是 dialog，会误判
    const isOutside = (event: PointerEvent) => {
      const rect = dialog.getBoundingClientRect()
      return (
        event.clientX < rect.left ||
        event.clientX > rect.right ||
        event.clientY < rect.top ||
        event.clientY > rect.bottom
      )
    }
    let pressedOutside = false
    const onPointerDown = (event: PointerEvent) => {
      pressedOutside = isOutside(event)
    }
    const onPointerUp = (event: PointerEvent) => {
      // 按下与抬起都在外面才关：从弹窗内拖选文本、抬手落到遮罩上的场景不应关闭
      if (pressedOutside && isOutside(event)) onCloseRef.current()
    }
    if (open && !SUPPORTS_CLOSEDBY) {
      dialog.addEventListener('pointerdown', onPointerDown)
      dialog.addEventListener('pointerup', onPointerUp)
    }

    // 打开瞬间先记录来源焦点：状态驱动的 close 通常发生在 React 已把节点移除之后
    // （useEffect 清理晚于 DOM 提交），浏览器失去原生还原机会，需要手动归还
    let prevFocus: HTMLElement | null = null
    if (open && !dialog.open) {
      prevFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null
      dialog.showModal()
    } else if (!open && dialog.open) {
      dialog.close()
    }

    return () => {
      dialog.removeEventListener('cancel', onCancel)
      dialog.removeEventListener('close', onCloseEvent)
      dialog.removeEventListener('pointerdown', onPointerDown)
      dialog.removeEventListener('pointerup', onPointerUp)
      if (dialog.open) {
        // 卸载兜底：父组件条件卸载时先走 close 让浏览器还原焦点，再移除节点；
        // 若节点已被 React 提前移除（弹窗带 open 属性脱离文档时浏览器只做 top layer 清理、
        // 不发 close 事件也不还焦点），手动归还给 showModal 前的焦点元素
        const detached = !dialog.isConnected
        dialog.close()
        if (detached && prevFocus?.isConnected) prevFocus.focus()
      }
    }
  }, [open])

  return dialogRef
}
