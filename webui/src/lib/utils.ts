import { type ClassValue, clsx } from 'clsx'
import { twMerge } from 'tailwind-merge'

/** shadcn-svelte's class merge helper (same as agent-webui). */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}
