import { useEffect, useRef } from 'react'
import { getJSON } from '../lib/api'
import { isAPITokenRole } from '../lib/auth'

/**
 * 按设置项 auto_refresh_interval_seconds 自动刷新（0 表示关闭）。
 * 与旧版 ui.js:createAutoRefresh 一致：API Token 角色不刷新；页面隐藏或有对话框打开时跳过本轮。
 * fixedSeconds 用于旧版本就写死间隔的页面（如趋势页 5 分钟）。
 */
export function useAutoRefresh(refresh: () => void, options: { fixedSeconds?: number; enabled?: boolean } = {}) {
  const refreshRef = useRef(refresh)
  useEffect(() => { refreshRef.current = refresh }, [refresh])
  const { fixedSeconds, enabled = true } = options
  useEffect(() => {
    if (!enabled || isAPITokenRole()) return
    let timer = 0
    let cancelled = false
    const start = (seconds: number) => {
      if (cancelled || !(seconds > 0)) return
      timer = window.setInterval(() => {
        if (document.hidden || document.querySelector('dialog[open]')) return
        refreshRef.current()
      }, Math.max(seconds, 5) * 1000)
    }
    if (fixedSeconds) start(fixedSeconds)
    else void getJSON<{ value?: string }>('/admin/settings/auto_refresh_interval_seconds').then((setting) => start(Number(setting?.value ?? 0))).catch(() => undefined)
    return () => { cancelled = true; window.clearInterval(timer) }
  }, [fixedSeconds, enabled])
}
