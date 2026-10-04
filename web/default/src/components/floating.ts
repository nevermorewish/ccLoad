import { useEffect, useLayoutEffect, useState, type CSSProperties, type RefObject } from 'react'

/**
 * 下拉浮层定位：按锚点的视口坐标使用 position:fixed，避免被 table-wrap / dialog-body 的
 * overflow 裁切。浮层渲染在锚点所在的 <dialog> 内，仍处于同一 top layer，不会被遮挡或 inert。
 */
export function useAnchoredPosition(open: boolean, anchorRef: RefObject<HTMLElement | null>, maxHeight = 260) {
  const [style, setStyle] = useState<CSSProperties>({})

  useLayoutEffect(() => {
    if (!open) return
    const update = () => {
      const anchor = anchorRef.current
      if (!anchor) return
      const rect = anchor.getBoundingClientRect()
      const below = window.innerHeight - rect.bottom
      // 下方空间不足且上方更宽裕时向上展开。
      const openUp = below < Math.min(maxHeight, 160) && rect.top > below
      setStyle({
        position: 'fixed',
        left: Math.max(4, Math.min(rect.left, window.innerWidth - Math.max(rect.width, 180) - 4)),
        width: Math.max(rect.width, 180),
        maxHeight: Math.max(120, Math.min(maxHeight, (openUp ? rect.top : below) - 12)),
        ...(openUp ? { top: 'auto', bottom: window.innerHeight - rect.top + 4 } : { top: rect.bottom + 4, bottom: 'auto' }),
      })
    }
    update()
    window.addEventListener('resize', update, true)
    window.addEventListener('scroll', update, true)
    return () => { window.removeEventListener('resize', update, true); window.removeEventListener('scroll', update, true) }
  }, [open, anchorRef, maxHeight])

  return style
}

/** 点击锚点容器外部时关闭；使用 mousedown 捕获阶段，与旧实现一致。 */
export function useOutsideClose(open: boolean, containerRef: RefObject<HTMLElement | null>, onClose: () => void) {
  useEffect(() => {
    if (!open) return
    const onDown = (event: MouseEvent) => { if (containerRef.current && !containerRef.current.contains(event.target as Node)) onClose() }
    document.addEventListener('mousedown', onDown, true)
    return () => document.removeEventListener('mousedown', onDown, true)
  }, [open, containerRef, onClose])
}
