import { useCallback, useEffect, useId, useMemo, useRef, useState, type KeyboardEvent } from 'react'
import { useAnchoredPosition, useOutsideClose } from './floating'

/**
 * 可搜索单选下拉。
 * 契约对齐 web/assets/js/searchable-select.js + ui.js:createSearchableCombobox：
 * role=combobox + aria-autocomplete=list + aria-controls/aria-expanded/aria-activedescendant，
 * ArrowDown/ArrowUp 移动、Enter 提交、Escape 取消并还原、blur 提交首个匹配项。
 * 隐藏的原生 <select> 承载表单/业务值，保证表单提交与既有取值逻辑不变。
 */
export type SelectOption = { value: string; label: string; disabled?: boolean }

export type SearchableSelectProps = {
  value: string
  options: SelectOption[]
  onChange: (value: string) => void
  placeholder?: string
  disabled?: boolean
  required?: boolean
  id?: string
  name?: string
  className?: string
  ariaLabel?: string
  allowCustomInput?: boolean
  /** 打开时未编辑则展示全部选项（filter-options 这类动态全集） */
  showAllOnOpen?: boolean
  emptyLabel?: string
}

export function SearchableSelect({ value, options, onChange, placeholder = '', disabled = false, required = false, id, name, className, ariaLabel, allowCustomInput = false, showAllOnOpen = true, emptyLabel = '' }: SearchableSelectProps) {
  const generatedId = useId()
  const selectId = id ?? `${generatedId}-native`
  const inputId = `${selectId}-input`
  const listboxId = `${selectId}-dropdown`
  const wrapRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [edited, setEdited] = useState(false)
  const [activeIndex, setActiveIndex] = useState(-1)

  const labelOf = (option: SelectOption | undefined) => option?.label ?? ''
  const selectedLabel = useMemo(() => labelOf(options.find((option) => option.value === value)), [options, value])

  // 外部值变化（载入编辑器、重置表单）时同步输入框显示文本。
  useEffect(() => { if (!open) setQuery(selectedLabel) }, [selectedLabel, open])

  const items = useMemo(() => {
    if (showAllOnOpen && !edited) return options
    const keyword = query.trim().toLowerCase()
    if (!keyword) return options
    return options.filter((option) => option.label.toLowerCase().includes(keyword) || option.value.toLowerCase().includes(keyword))
  }, [options, query, edited, showAllOnOpen])

  const close = () => { setOpen(false); setActiveIndex(-1); setEdited(false); setQuery(selectedLabel) }

  const commit = (option: SelectOption | undefined) => {
    if (option && option.disabled !== true) {
      if (option.value !== value) onChange(option.value)
      setQuery(option.label)
    } else if (allowCustomInput && edited) {
      const keyword = query.trim()
      if (keyword !== value) onChange(keyword)
      setQuery(keyword || selectedLabel)
    }
    setOpen(false); setActiveIndex(-1); setEdited(false)
  }

  const commitFirstMatched = () => {
    const keyword = query.trim()
    if (!keyword) { commit(items.find((option) => option.disabled !== true)); return }
    if (allowCustomInput) {
      const exact = items.find((option) => option.disabled !== true && option.label.toLowerCase() === keyword.toLowerCase())
      commit(exact ?? { value: keyword, label: keyword })
      return
    }
    commit(items.find((option) => option.disabled !== true))
  }

  const cancel = () => close()

  const move = (delta: number) => {
    if (!items.length) return
    let next = activeIndex
    if (next === -1) next = delta < 0 ? items.length : -1
    for (;;) {
      next += delta
      if (next < 0 || next >= items.length) { setActiveIndex(-1); return }
      if (items[next].disabled !== true) break
    }
    setActiveIndex(next)
  }

  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Escape') { if (open) { event.preventDefault(); event.stopPropagation(); cancel() } return }
    if (event.key === 'ArrowDown') { event.preventDefault(); if (!open) { setOpen(true); setEdited(false); return } move(1); return }
    if (event.key === 'ArrowUp') { event.preventDefault(); if (!open) { setOpen(true); setEdited(false); return } move(-1); return }
    if (event.key === 'Enter') {
      event.preventDefault()
      if (!open) return
      if (activeIndex >= 0 && activeIndex < items.length) { commit(items[activeIndex]); return }
      commitFirstMatched()
    }
  }

  // 点击外部关闭（mousedown 捕获阶段）；下拉按输入框视口坐标 fixed 定位，避免被 overflow 裁切。
  const closeOutside = useCallback(() => { setOpen(false); setActiveIndex(-1); setEdited(false) }, [])
  useOutsideClose(open, wrapRef, closeOutside)
  const listStyle = useAnchoredPosition(open, inputRef)

  return (
    <div ref={wrapRef} className={`combobox${className ? ` ${className}` : ''}`}>
      {/* 隐藏原生 select 承载表单值；CLAUDE.md 约定它只作业务值载体。 */}
      <select id={selectId} name={name} className="combobox-native" tabIndex={-1} aria-hidden="true" value={value} disabled={disabled} required={required} onChange={(event) => onChange(event.target.value)}>
        {allowCustomInput && value !== '' && !options.some((option) => option.value === value) && <option value={value}>{value}</option>}
        {options.map((option) => <option key={option.value || '__empty'} value={option.value} disabled={option.disabled}>{option.label}</option>)}
      </select>
      <input
        ref={inputRef}
        id={inputId}
        type="text"
        className="input combobox-input"
        role="combobox"
        aria-autocomplete="list"
        aria-haspopup="listbox"
        aria-controls={listboxId}
        aria-expanded={open}
        aria-activedescendant={open && activeIndex >= 0 ? `${listboxId}-option-${activeIndex}` : undefined}
        aria-label={ariaLabel}
        aria-required={required}
        aria-disabled={disabled}
        autoComplete="off"
        spellCheck={false}
        placeholder={placeholder}
        disabled={disabled}
        value={open ? query : selectedLabel || emptyLabel}
        onFocus={() => { if (!open) { setOpen(true); setEdited(false); setQuery('') } }}
        onChange={(event) => { setQuery(event.target.value); setEdited(true); setOpen(true); setActiveIndex(-1) }}
        onKeyDown={handleKeyDown}
        onBlur={() => { if (open) commitFirstMatched() }}
      />
      {open && <div id={listboxId} className="combobox-list" role="listbox" style={listStyle}>
        {items.length === 0 && <div className="combobox-empty muted">无匹配项</div>}
        {items.map((option, index) => (
          <div
            key={`${option.value}-${index}`}
            id={`${listboxId}-option-${index}`}
            className={`combobox-option${option.value === value ? ' selected' : ''}${index === activeIndex ? ' active' : ''}${option.disabled ? ' disabled' : ''}`}
            role="option"
            aria-selected={option.value === value}
            aria-disabled={option.disabled}
            // 用 mousedown 提交，避免 blur 先于 click 触发导致选不中。
            onMouseDown={(event) => { event.preventDefault(); commit(option) }}
          >{option.label}</div>
        ))}
      </div>}
    </div>
  )
}
