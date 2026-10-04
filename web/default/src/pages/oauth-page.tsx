import { OAuthLoginPanel } from './oauth/oauth-panels'

export function OAuthPage() {
  return <>
    <header className="page-header"><div><h1>OAuth 凭证</h1><p className="muted">选择认证类型，生成授权链接或直接导入凭证</p></div></header>
    <section className="card"><OAuthLoginPanel /></section>
  </>
}
