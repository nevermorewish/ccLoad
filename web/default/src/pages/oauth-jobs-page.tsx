import { OAuthCleanupPanel, OAuthImportPanel } from './oauth/oauth-panels'

export function OAuthJobsPage() {
  return <>
    <header className="page-header"><div><h1>OAuth 批量任务</h1><p className="muted">批量导入凭证，或按认证类型和模型清理失效凭证</p></div></header>
    <section className="card"><h2>批量导入凭证</h2><OAuthImportPanel /></section>
    <section className="card" style={{ marginTop: 16 }}><h2>凭证清理</h2><OAuthCleanupPanel /></section>
  </>
}
