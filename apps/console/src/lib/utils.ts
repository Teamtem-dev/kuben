import { type ClassValue, clsx } from 'clsx'
import { twMerge } from 'tailwind-merge'

/** Class-name merging for Tailwind (shadcn's `cn`): later classes win. */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}
