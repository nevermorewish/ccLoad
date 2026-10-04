import { createRoot } from 'react-dom/client'
import { lazy, StrictMode, Suspense, useEffect, useState } from 'react'
import { createRootRoute, createRoute, createRouter, Link, Outlet, RouterProvider } from '@tanstack/react-router'
import { getSession, login } from './lib/auth'
import { AppLayout } from './components/layout'
import type { Session } from './types'
import './styles/index.css'

// 页面按路由懒加载，首屏只下载概览和应用壳，避免渠道/日志/模型测试等大页面
// 在用户尚未访问时阻塞首屏。Rsbuild 生产构建会为这些入口生成独立缓存块。
const DashboardPage = lazy(() => import('./pages/dashboard-page').then((module) => ({ default: module.DashboardPage })))
const ChannelsPage = lazy(() => import('./pages/channels-page').then((module) => ({ default: module.ChannelsPage })))
const MonitorPage = lazy(() => import('./pages/monitor-page').then((module) => ({ default: module.MonitorPage })))
const StatsPage = lazy(() => import('./pages/stats-page').then((module) => ({ default: module.StatsPage })))
const TrendPage = lazy(() => import('./pages/trend-page').then((module) => ({ default: module.TrendPage })))
const ModelTestPage = lazy(() => import('./pages/model-test-page').then((module) => ({ default: module.ModelTestPage })))
const LogsPage = lazy(() => import('./pages/logs-page').then((module) => ({ default: module.LogsPage })))
const TokensPage = lazy(() => import('./pages/tokens-page').then((module) => ({ default: module.TokensPage })))
const SettingsPage = lazy(() => import('./pages/settings-page').then((module) => ({ default: module.SettingsPage })))
const OAuthPage = lazy(() => import('./pages/oauth-page').then((module) => ({ default: module.OAuthPage })))
const ModelCatalogPage = lazy(() => import('./pages/model-catalog-page').then((module) => ({ default: module.ModelCatalogPage })))
const OAuthJobsPage = lazy(() => import('./pages/oauth-jobs-page').then((module) => ({ default: module.OAuthJobsPage })))

function Header({ title, description, onRefresh }: { title: string; description?: string; onRefresh?: () => void }) {
  return <header className="page-header"><div><h1>{title}</h1>{description && <p className="muted">{description}</p>}</div>{onRefresh && <button className="btn" onClick={onRefresh}>刷新</button>}</header>
}

function Login() {
  const [mode, setMode] = useState<'admin' | 'api_token'>('admin')
  const [credential, setCredential] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const submit = async (event: React.FormEvent) => {
    event.preventDefault(); setBusy(true); setError(null)
    try { await login(mode, credential); window.location.href = '/web/' }
    catch (cause) { setError(cause instanceof Error ? cause.message : '登录失败') }
    finally { setBusy(false) }
  }
  return <div className="login-shell"><form className="card login-card" onSubmit={(event) => void submit(event)}><div className="brand login-brand"><span className="brand-mark">C</span><span>ccLoad</span></div><h1>登录管理控制台</h1><div className="toolbar"><button type="button" className={mode === 'admin' ? 'btn btn-primary' : 'btn'} onClick={() => setMode('admin')}>管理员密码</button><button type="button" className={mode === 'api_token' ? 'btn btn-primary' : 'btn'} onClick={() => setMode('api_token')}>API Token</button></div><input className="input wide" autoFocus type={mode === 'admin' ? 'password' : 'text'} value={credential} onChange={(event) => setCredential(event.target.value)} placeholder={mode === 'admin' ? '管理员密码' : 'API 访问令牌'} /><button className="btn btn-primary wide" disabled={busy || !credential.trim()}>{busy ? '登录中...' : '登录'}</button>{error && <p className="error-text">{error}</p>}</form></div>
}


const rootRoute = createRootRoute({ component: () => <App /> })
const dashboardRoute = createRoute({ getParentRoute: () => rootRoute, path: '/', component: DashboardPage })
const channelsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/channels', component: ChannelsPage })
const monitorRoute = createRoute({ getParentRoute: () => rootRoute, path: '/monitor', component: MonitorPage })
const statsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/stats', component: StatsPage })
const trendRoute = createRoute({ getParentRoute: () => rootRoute, path: '/trend', component: TrendPage })
const modelTestRoute = createRoute({ getParentRoute: () => rootRoute, path: '/model-test', component: ModelTestPage })
const logsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/logs', component: LogsPage })
const tokensRoute = createRoute({ getParentRoute: () => rootRoute, path: '/tokens', component: TokensPage })
const settingsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/settings', component: SettingsPage })
const oauthRoute = createRoute({ getParentRoute: () => rootRoute, path: '/oauth', component: OAuthPage })
const modelCatalogRoute = createRoute({ getParentRoute: () => rootRoute, path: '/model-catalog', component: ModelCatalogPage })
const oauthJobsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/oauth-jobs', component: OAuthJobsPage })
const loginRoute = createRoute({ getParentRoute: () => rootRoute, path: '/login', component: Login })
const routeTree = rootRoute.addChildren([dashboardRoute, channelsRoute, monitorRoute, statsRoute, trendRoute, modelTestRoute, logsRoute, tokensRoute, settingsRoute, oauthRoute, modelCatalogRoute, oauthJobsRoute, loginRoute])
const router = createRouter({ routeTree, basepath: '/web', defaultPreload: 'intent' })

function App() { const [session, setSession] = useState<Session | null | undefined>(undefined); const path = window.location.pathname.replace(/^\/web(?=\/|$)/, '') || '/'; useEffect(() => { if (path !== '/login') void getSession().then(setSession); else setSession(null) }, [path]); if (path === '/login') return <Outlet />; return session ? <AppLayout><Suspense fallback={<div className="page-shell"><Header title="ccLoad" /><div className="card">正在加载页面...</div></div>}><Outlet /></Suspense></AppLayout> : <div className="page-shell"><Header title="ccLoad" /><div className="card">{session === undefined ? '正在验证登录状态...' : <Link className="btn btn-primary" to="/login">前往登录</Link>}</div></div> }

declare module '@tanstack/react-router' { interface Register { router: typeof router } }

createRoot(document.getElementById('root')!).render(<StrictMode><RouterProvider router={router} /></StrictMode>)
