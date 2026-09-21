/**
 * A drop-in for `react-style-singleton` (used by react-remove-scroll, which
 * Radix Dialog, Sheet, Drawer and the menus use to lock page scroll) that adds
 * its rules as a constructed stylesheet (`document.adoptedStyleSheets`)
 * instead of a style element. Kuben's content security policy
 * (`style-src 'self'`) blocks style elements; the CSSOM is allowed.
 * vite.config.ts aliases the package to this module. Same API and semantics:
 * the first instance's styles win, the last one to unmount removes them.
 */
import { type FC, useEffect } from 'react'

function adopt(sheet: CSSStyleSheet) {
  document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheet]
}

function drop(sheet: CSSStyleSheet) {
  document.adoptedStyleSheets = document.adoptedStyleSheets.filter((s) => s !== sheet)
}

export const stylesheetSingleton = () => {
  let counter = 0
  let sheet: CSSStyleSheet | null = null
  return {
    add: (style: string) => {
      if (counter === 0) {
        sheet = new CSSStyleSheet()
        sheet.replaceSync(style)
        adopt(sheet)
      }
      counter++
    },
    remove: () => {
      counter--
      if (counter === 0 && sheet) {
        drop(sheet)
        sheet = null
      }
    },
  }
}

export const styleHookSingleton = () => {
  const sheet = stylesheetSingleton()
  return (styles: string, isDynamic?: boolean) => {
    // Same dependency as upstream: re-applied only for dynamic styles.
    useEffect(() => {
      sheet.add(styles)
      return () => sheet.remove()
    }, [styles && isDynamic])
  }
}

export const styleSingleton = (): FC<{ styles: string; dynamic?: boolean }> => {
  const useStyle = styleHookSingleton()
  return ({ styles, dynamic }) => {
    useStyle(styles, dynamic)
    return null
  }
}
