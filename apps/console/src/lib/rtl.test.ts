import { describe, expect, test } from 'bun:test'
import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { logical, mirroredTranslate, rewriteClasses, rewriteSource } from '../../scripts/rtl-codemod'

describe('rtl codemod', () => {
  test('physical utilities become logical, variants and signs kept', () => {
    expect(logical('ml-auto')).toBe('ms-auto')
    expect(logical('-mr-1')).toBe('-me-1')
    expect(logical('data-[inset]:pl-8')).toBe('data-[inset]:ps-8')
    expect(logical('group-data-[side=left]:-right-4')).toBe('group-data-[side=left]:-inset-e-4')
    expect(logical('sm:text-left')).toBe('sm:text-start')
    expect(logical('rounded-tl-sm')).toBe('rounded-ss-sm')
    expect(logical('last:rounded-r-md')).toBe('last:rounded-e-md')
    expect(logical('border-l')).toBe('border-s')
    expect(logical('[&>*:not(:first-child)]:border-l-0')).toBe('[&>*:not(:first-child)]:border-s-0')
    expect(logical('data-[state=open]:slide-in-from-left')).toBe('data-[state=open]:slide-in-from-start')
  })

  test('physical placements stay physical', () => {
    expect(logical('data-[side=left]:slide-in-from-right-2')).toBe('data-[side=left]:slide-in-from-right-2')
    expect(logical('data-[vaul-drawer-direction=left]:left-0')).toBe(
      'data-[vaul-drawer-direction=left]:left-0',
    )
    expect(logical('transition-[left,right,width]')).toBe('transition-[left,right,width]')
  })

  test('translate-x gains a mirrored rtl twin', () => {
    expect(mirroredTranslate('-translate-x-1/2')).toBe('rtl:translate-x-1/2')
    expect(mirroredTranslate('translate-x-[-50%]')).toBe('rtl:translate-x-[50%]')
    expect(mirroredTranslate('data-[state=checked]:translate-x-[calc(100%-2px)]')).toBe(
      'rtl:data-[state=checked]:-translate-x-[calc(100%-2px)]',
    )
    expect(mirroredTranslate('translate-x-0')).toBeUndefined()
    expect(mirroredTranslate('data-[side=left]:-translate-x-1')).toBeUndefined()
    expect(rewriteClasses('left-1/2 -translate-x-1/2')).toBe(
      'inset-s-1/2 -translate-x-1/2 rtl:translate-x-1/2',
    )
  })

  test('is idempotent', () => {
    const interpolation = ['$', '{x}'].join('')
    const once = rewriteSource(`cn("fixed left-[50%] translate-x-[-50%] pl-2", \`mr-1 ${interpolation}\`)`)
    expect(rewriteSource(once)).toBe(once)
  })

  test('every generated component is already logical', () => {
    const dir = join(import.meta.dir, '..', 'components', 'ui')
    for (const name of readdirSync(dir).filter((n) => n.endsWith('.tsx'))) {
      const source = readFileSync(join(dir, name), 'utf8')
      expect({ name, logical: rewriteSource(source) === source }).toEqual({ name, logical: true })
    }
  })
})
