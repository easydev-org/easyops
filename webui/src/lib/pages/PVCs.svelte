<script lang="ts">
  import { onMount } from 'svelte'
  import Button from '$lib/components/ui/button.svelte'
  import Badge from '$lib/components/ui/badge.svelte'
  import type { Api, PVC } from '$lib/api'
  import { badgeVariant } from '$lib/format'
  let { api }: { api: Api } = $props()
  let items = $state<PVC[]>([])
  let err = $state('')
  async function load() {
    err = ''
    try {
      items = (await api.pvcs()).pvcs ?? []
    } catch (e) {
      err = String(e)
    }
  }
  onMount(load)
</script>
{#if err}<p class="mb-3 text-meta text-destructive">{err}</p>{/if}
<div class="mb-3 flex items-center gap-2"><h3 class="text-body font-semibold">PersistentVolumeClaims</h3><Button variant="outline" size="sm" onclick={load}>refresh</Button></div>
<div class="overflow-hidden rounded-lg border border-border bg-card">
  <table class="w-full border-collapse">
    <thead><tr class="border-b border-border text-left text-micro uppercase tracking-wider text-muted-foreground">
      <th class="px-4 py-3 font-medium">name</th><th class="px-4 py-3 font-medium">size</th><th class="px-4 py-3 font-medium">class</th>
      <th class="px-4 py-3 font-medium">phase</th><th class="px-4 py-3 font-medium">mounted by</th>
    </tr></thead>
    <tbody>
      {#each items as p (p.name)}
        <tr class="border-b border-border/60 last:border-0 hover:bg-secondary">
          <td class="px-4 py-2.5 text-meta font-medium">{p.name}</td>
          <td class="px-4 py-2.5 text-meta">{p.size}</td>
          <td class="px-4 py-2.5 text-meta text-muted-foreground">{p.storageClass}</td>
          <td class="px-4 py-2.5"><Badge variant={badgeVariant(p.phase)}>{p.phase}</Badge></td>
          <td class="px-4 py-2.5 text-meta text-muted-foreground">{(p.mountedBy ?? []).join(', ')}</td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>
