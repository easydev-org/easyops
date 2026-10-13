<script lang="ts">
  import { cn } from '$lib/utils'

  let {
    value = $bindable(''),
    placeholder = '',
    type = 'text',
    class: klass = '',
  }: { value?: string; placeholder?: string; type?: string; class?: string } = $props()
</script>

<input
  {type}
  bind:value
  {placeholder}
  class={cn(
    'h-8 rounded-md border border-input bg-secondary px-3 text-meta outline-none placeholder:text-muted-foreground focus:border-ring focus:ring-2 focus:ring-ring/25',
    klass,
  )}
/>
