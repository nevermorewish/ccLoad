/**
 * Copy text while keeping a synchronous fallback for browsers and deployments
 * where the Clipboard API is unavailable (for example, an HTTP admin page).
 *
 * The fallback must run before awaiting navigator.clipboard.writeText: waiting
 * first can consume the click's user activation and make execCommand fail.
 */
function copyWithExecCommand(text: string): boolean {
  if (typeof document === 'undefined' || typeof document.execCommand !== 'function') return false
  const textarea = document.createElement('textarea')
  textarea.value = text
  textarea.readOnly = true
  textarea.setAttribute('aria-hidden', 'true')
  textarea.style.position = 'fixed'
  textarea.style.top = '0'
  textarea.style.left = '-9999px'
  textarea.style.opacity = '0'
  const host = document.body
  if (!host) return false
  const previous = document.activeElement as HTMLElement | null
  host.appendChild(textarea)
  try {
    textarea.focus({ preventScroll: true })
    textarea.select()
    textarea.setSelectionRange(0, textarea.value.length)
    return document.execCommand('copy')
  } catch {
    return false
  } finally {
    textarea.remove()
    previous?.focus?.({ preventScroll: true })
  }
}

export function copyToClipboard(text: string): Promise<void> {
  if (copyWithExecCommand(text)) return Promise.resolve()
  const clipboard = typeof navigator !== 'undefined' ? navigator.clipboard : undefined
  if (clipboard && typeof clipboard.writeText === 'function') return clipboard.writeText(text)
  return Promise.reject(new Error('Clipboard API is unavailable'))
}
