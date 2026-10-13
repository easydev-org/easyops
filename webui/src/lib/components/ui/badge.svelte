<script lang="ts">
  import { cn } from '$lib/utils'
  import type { Snippet } from 'svelte'

  let {
    variant = 'muted',
    class: klass = '',
    children,
  }: {
    variant?: 'muted' | 'accent' | 'success' | 'warning' | 'destructive'
    class?: string
    children: Snippet
  } = $props()

  const variants: Record<string, string> = {
    muted: 'bg-secondary text-muted-foreground',
    accent: 'bg-accent/15 text-accent',
    success: 'bg-success/15 text-success',
    warning: 'bg-warning/15 text-warning',
    destructive: 'bg-destructive/15 text-destructive',
  }
</script>

<span class={cn('inline-flex items-center rounded-sm px-1.5 py-0.5 text-micro font-medium', variants[variant], klass)}>
  {@render children()}
</span>
