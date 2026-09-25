/**
 * The operational views' old helpers, now drawn by the kit; this file goes
 * once the last page imports from `@/components/kit` directly.
 */
import { Copyable as KitCopyable } from './kit'

export { Sparkline, ToneBadge as Pill } from './kit'

export function Copyable({ value }: { value: string; label: string }) {
  return <KitCopyable value={value} />
}
