<script lang="ts">
  import { cn } from '$lib/utils'
  import type { Snippet } from 'svelte'

  let {
    variant = 'default',
    size = 'default',
    class: klass = '',
    onclick,
    children,
  }: {
    variant?: 'default' | 'outline' | 'ghost'
    size?: 'default' | 'sm'
    class?: string
    onclick?: (e: MouseEvent) => void
    children: Snippet
  } = $props()

  const variants: Record<string, string> = {
    default: 'bg-primary text-primary-foreground hover:opacity-90',
    outline: 'border border-border bg-transparent hover:bg-secondary',
    ghost: 'hover:bg-secondary',
  }
  const sizes: Record<string, string> = { default: 'h-8 px-3.5', sm: 'h-7 px-2.5' }
</script>

<button
  type="button"
  class={cn(
    'inline-flex items-center justify-center gap-1.5 rounded-md text-meta font-medium transition focus:outline-none focus-visible:ring-2 focus-visible:ring-ring/40',
    variants[variant],
    sizes[size],
    klass,
  )}
  {onclick}
>
  {@render children()}
</button>
