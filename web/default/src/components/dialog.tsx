import { useCallback, useEffect, useRef, type MouseEvent, type ReactNode } from 'react'

/** 原生 <dialog> 基座：showModal 顶层渲染、Esc、遮罩点击关闭、焦点归还、脏表单确认。 */
export type DialogProps = {
  open: boolean
  onClose: () => void
  title?: ReactNode
  description?: ReactNode
  footer?: ReactNode
  size?: 'sm' | 'md' | 'lg' | 'xl'
  className?: string
  /** 有未保存改动时，关闭前二次确认 */
  confirmOnClose?: boolean
  confirmMessage?: string
  closeOnBackdrop?: boolean
  children?: ReactNode
}

export function Dialog({ open, onClose, title, description, footer, size = 'md', className, confirmOnClose = false, confirmMessage = '有未保存的改动，确定关闭吗？', closeOnBackdrop = true, children }: DialogProps) {
  const dialogRef = useRef<HTMLDialogElement>(null)
  const restoreRef = useRef<HTMLElement | null>(null)
  const closeRef = useRef(onClose)

  useEffect(() => { closeRef.current = onClose }, [onClose])

  const requestClose = useCallback(() => {
    if (confirmOnClose && !window.confirm(confirmMessage)) return
    closeRef.current()
  }, [confirmOnClose, confirmMessage])

  // React 不接管 <dialog open>；开关一律走命令式 API，避免属性与顶层状态不同步。
  useEffect(() => {
    const element = dialogRef.current
    if (!element) return
    if (open) {
      if (element.open) return
      restoreRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
      element.showModal()
      return
    }
    if (element.open) element.close()
    restoreRef.current?.focus()
    restoreRef.current = null
  }, [open])

  // Esc 触发原生 cancel；拦下后统一走 requestClose，保证脏表单确认不被绕过。
  useEffect(() => {
    if (!open) return
    const element = dialogRef.current
    if (!element) return
    const onCancel = (event: Event) => { event.preventDefault(); requestClose() }
    element.addEventListener('cancel', onCancel)
    return () => element.removeEventListener('cancel', onCancel)
  }, [open, requestClose])

  const handleBackdropClick = (event: MouseEvent<HTMLDialogElement>) => {
    if (!closeOnBackdrop || event.target !== dialogRef.current) return
    requestClose()
  }

  return (
    <dialog ref={dialogRef} className={`dialog dialog-${size}${className ? ` ${className}` : ''}`} onClick={handleBackdropClick}>
      {open && <div className="dialog-panel">
        <header className="dialog-header">
          <div className="dialog-heading">
            {title && <h2 className="dialog-title">{title}</h2>}
            {description && <p className="dialog-description muted">{description}</p>}
          </div>
          <button type="button" className="dialog-close" aria-label="关闭" onClick={requestClose}>×</button>
        </header>
        <div className="dialog-body">{children}</div>
        {footer && <footer className="dialog-footer">{footer}</footer>}
      </div>}
    </dialog>
  )
}
