<script lang="ts">
  import { onMount } from 'svelte'
  import { Boxes, Container, HardDrive, Package } from '@lucide/svelte'
  import Badge from '$lib/components/ui/badge.svelte'
  import type { Api } from '$lib/api'
  import { badgeVariant } from '$lib/format'

  let { api }: { api: Api } = $props()
  let s = $state({ services: 0, sandboxes: 0, pvcs: 0, helm: 0, unhealthy: 0 })
  let err = $state('')
  onMount(async () => {
    try {
      const [svc, sb, pvc, helm] = await Promise.all([api.services(), api.sandboxes(), api.pvcs(), api.helm()])
      const services = svc.services ?? []
      const sandboxes = sb.sandboxes ?? []
      s = {
        services: services.length,
        sandboxes: sandboxes.length,
        pvcs: (pvc.pvcs ?? []).length,
        helm: (helm.releases ?? []).length,
        unhealthy: [...services, ...sandboxes].filter((x) => x.phase && badgeVariant(x.phase) !== 'success').length,
      }
    } catch (e) {
      err = String(e)
    }
  })
</script>

{#if err}<p class="mb-3 text-meta text-destructive">{err}</p>{/if}
<div class="grid grid-cols-2 gap-3 lg:grid-cols-4">
  {#each [['services', s.services, Container], ['sandboxes', s.sandboxes, Boxes], ['pvcs', s.pvcs, HardDrive], ['helm', s.helm, Package]] as [label, value, Icon] (label)}
    <div class="rounded-lg border border-border bg-card p-4">
      <div class="flex items-center gap-1.5 text-micro uppercase tracking-wider text-muted-foreground"><Icon class="size-3.5" /> {label}</div>
      <p class="mt-1.5 text-xl font-semibold tabular-nums">{value}</p>
    </div>
  {/each}
</div>
<div class="mt-6 rounded-lg border border-border bg-card p-4">
  <p class="text-meta">Unhealthy services + sandboxes: <span class={s.unhealthy ? 'text-warning font-semibold' : 'text-success font-semibold'}>{s.unhealthy}</span></p>
</div>
