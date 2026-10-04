import { useCallback, useId, useMemo, useRef, useState } from 'react'
import { useAnchoredPosition, useOutsideClose } from './floating'

/**
 * 可搜索多选。CLAUDE.md 要求多选控件必须提供可搜索实现，这里补齐。
 * 隐藏的原生多选 <select> 承载表单值，交互层复用与 SearchableSelect 相同的键盘契约。
 */
export type MultiSelectOption = { value: string; label: string; disabled?: boolean }

export type SearchableMultiSelectProps = {
  values: string[]
  options: MultiSelectOption[]
  onChange: (values: string[]) => void
  placeholder?: string
  disabled?: boolean
  id?: string
  name?: string
  className?: string
  ariaLabel?: string
  /** 空选择是否表示“全部允许” */
  emptyMeansAll?: boolean
}

export function SearchableMultiSelect({ values, options, onChange, placeholder = '', disabled = false, id, name, className, ariaLabel, emptyMeansAll = false }: SearchableMultiSelectProps) {
  const generatedId = useId()
  const selectId = id ?? `${generatedId}-native`
  const listboxId = `${selectId}-dropdown`
  const wrapRef = useRef<HTMLDivElement>(null)
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')

  const selected = useMemo(() => new Set(values), [values])
  const items = useMemo(() => {
    const keyword = query.trim().toLowerCase()
    if (!keyword) return options
    return options.filter((option) => option.label.toLowerCase().includes(keyword) || option.value.toLowerCase().includes(keyword))
  }, [options, query])

  const triggerRef = useRef<HTMLButtonElement>(null)
  const closeOutside = useCallback(() => setOpen(false), [])
  useOutsideClose(open, wrapRef, closeOutside)
  const listStyle = useAnchoredPosition(open, triggerRef, 300)

  const toggle = (option: MultiSelectOption) => {
    if (option.disabled) return
    onChange(selected.has(option.value) ? values.filter((item) => item !== option.value) : [...values, option.value])
  }

  const summary = values.length === 0
    ? (emptyMeansAll ? '全部允许' : placeholder || '未选择')
    : `已选 ${values.length} 项`

  return (
    <div ref={wrapRef} className={`combobox multiselect${className ? ` ${className}` : ''}`} onKeyDown={(event) => {
      // 下拉打开时 Esc 只关闭下拉，不让外层 <dialog> 收到关闭请求。
      if (event.key === 'Escape' && open) { event.preventDefault(); event.stopPropagation(); setOpen(false); triggerRef.current?.focus() }
    }}>
      {/* 隐藏原生多选，仅承载表单值。 */}
      <select id={selectId} name={name} className="combobox-native" multiple tabIndex={-1} aria-hidden="true" value={values} disabled={disabled} onChange={() => undefined}>
        {options.map((option) => <option key={option.value} value={option.value} disabled={option.disabled}>{option.label}</option>)}
      </select>
      <button
        ref={triggerRef}
        type="button"
        className="input combobox-input multiselect-trigger"
        aria-haspopup="listbox"
        aria-controls={listboxId}
        aria-expanded={open}
        aria-label={ariaLabel}
        aria-disabled={disabled}
        disabled={disabled}
        onClick={() => { setOpen(!open); setQuery('') }}
      ><span className={values.length === 0 ? 'muted' : undefined}>{summary}</span></button>
      {open && <div id={listboxId} className="combobox-list" role="listbox" aria-multiselectable="true" style={listStyle}>
        <input className="input combobox-input multiselect-search" value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索" autoFocus />
        {items.length === 0 && <div className="combobox-empty muted">无匹配项</div>}
        {items.map((option) => (
          <div
            key={option.value}
            className={`combobox-option${selected.has(option.value) ? ' selected' : ''}${option.disabled ? ' disabled' : ''}`}
            role="option"
            aria-selected={selected.has(option.value)}
            aria-disabled={option.disabled}
            onMouseDown={(event) => { event.preventDefault(); toggle(option) }}
          >
            <input type="checkbox" checked={selected.has(option.value)} disabled={option.disabled} readOnly tabIndex={-1} />
            <span>{option.label}</span>
          </div>
        ))}
        {values.length > 0 && <div className="multiselect-footer"><button type="button" className="link-button" onClick={() => onChange([])}>清空选择</button></div>}
      </div>}
    </div>
  )
}
