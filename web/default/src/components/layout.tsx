import { Link, useLocation } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { BarChart3, Gauge, Home, KeyRound, List, LogOut, Settings, Activity, Sun, Moon, Languages } from 'lucide-react'
import type { ReactNode } from 'react'
import { logout } from '../lib/auth'

const links = [
  { to: '/', label: '概览', icon: Home },
  { to: '/monitor', label: '渠道监控', icon: Activity },
  { to: '/channels', label: '渠道', icon: List },
  { to: '/stats', label: '统计', icon: BarChart3 },
  { to: '/trend', label: '趋势', icon: BarChart3 },
  { to: '/model-test', label: '模型测试', icon: Gauge },
  { to: '/logs', label: '日志', icon: Gauge },
  { to: '/tokens', label: '令牌', icon: KeyRound },
  { to: '/settings', label: '设置', icon: Settings },
  { to: '/oauth', label: 'OAuth', icon: KeyRound },
  { to: '/model-catalog', label: '模型目录', icon: List },
  { to: '/oauth-jobs', label: 'OAuth 任务', icon: Activity },
] as const

export function AppLayout({ children }: { children: ReactNode }) {
  const location = useLocation()
  const [theme, setTheme] = useState(() => localStorage.getItem('ccload_theme') ?? 'dark')
  const [locale, setLocale] = useState(() => localStorage.getItem('ccload_locale') ?? 'zh-CN')
  useEffect(() => { document.documentElement.dataset.resolvedTheme = theme; document.documentElement.style.colorScheme = theme; localStorage.setItem('ccload_theme', theme) }, [theme])
  useEffect(() => { document.documentElement.lang = locale; localStorage.setItem('ccload_locale', locale) }, [locale])
  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div className="brand"><span className="brand-mark">C</span><span>ccLoad</span></div>
        <nav>{links.map(({ to, label, icon: Icon }) => (
          <Link key={to} to={to} className={location.pathname === to ? 'nav-link active' : 'nav-link'}>
            <Icon size={17} />{label}
          </Link>
        ))}</nav>
        <div className="sidebar-tools"><button className="nav-link" title="切换主题" onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}>{theme === 'dark' ? <Sun size={17} /> : <Moon size={17} />}<span>主题</span></button><button className="nav-link" title="切换语言" onClick={() => setLocale(locale === 'zh-CN' ? 'en-US' : 'zh-CN')}><Languages size={17} /><span>{locale === 'zh-CN' ? '中文' : 'English'}</span></button></div>
        <button className="nav-link logout" onClick={() => { logout(); location.href = '/web/' }}>
          <LogOut size={17} />退出登录
        </button>
      </aside>
      <main className="page-shell">{children}</main>
    </div>
  )
}
