import { createContext, useContext } from 'react'
import type { Session } from '@/types'

/** 当前登录会话；页面据此决定只读模式与是否隐藏渠道维度。 */
export const SessionContext = createContext<Session | null>(null)
export const useSession = () => useContext(SessionContext)
