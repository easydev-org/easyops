<script lang="ts">
  import { onMount } from 'svelte'
  import { Container, Terminal, X } from '@lucide/svelte'
  import Button from '$lib/components/ui/button.svelte'
  import Badge from '$lib/components/ui/badge.svelte'
  import type { Api, Service } from '$lib/api'
  import { badgeVariant } from '$lib/format'

  let { api }: { api: Api } = $props()
  let services = $state<Service[]>([])
  let err = $state('')
  let sel = $state<Service | null>(null)
  let logs = $state<string[]>([])
  let logsErr = $state('')

  async function load() {
    err = ''
    try {
      services = (await api.services()).services ?? []
    } catch (e) {
      err = String(e)
    }
  }
  onMount(load)
  async function openLogs(s: Service) {
    sel = s
    logs = []
    logsErr = ''
    try {
      logs = (await api.serviceLogs(s.owner ?? '', s.name)).lines ?? []
    } catch (e) {
      logsErr = String(e)
    }
  }
</script>

{#if err}<p class="mb-3 text-meta text-destructive">{err}</p>{/if}
<div class="mb-3 flex items-center gap-2"><h3 class="text-body font-semibold">Services</h3><Button variant="outline" size="sm" onclick={load}>refresh</Button></div>
<div class="overflow-hidden rounded-lg border border-border bg-card">
  <table class="w-full border-collapse">
    <thead><tr class="border-b border-border text-left text-micro uppercase tracking-wider text-muted-foreground">
      <th class="px-4 py-3 font-medium">name</th><th class="px-4 py-3 font-medium">image</th><th class="px-4 py-3 font-medium">state</th>
      <th class="px-4 py-3 font-medium">replicas</th><th class="px-4 py-3 font-medium">owner</th><th class="px-4 py-3"></th>
    </tr></thead>
    <tbody>
      {#each services as s (s.name)}
        <tr class="border-b border-border/60 last:border-0 hover:bg-secondary">
          <td class="px-4 py-2.5 text-meta font-medium">{s.name}</td>
          <td class="px-4 py-2.5 font-mono text-micro text-muted-foreground">{s.image}</td>
          <td class="px-4 py-2.5"><Badge variant={badgeVariant(s.phase)}>{s.phase}</Badge>{#if s.message}<span class="ml-2 text-micro text-destructive">{s.message}</span>{/if}</td>
          <td class="px-4 py-2.5 text-meta tabular-nums">{s.readyReplicas ?? 0}/{s.replicas ?? 0}</td>
          <td class="px-4 py-2.5 text-meta text-muted-foreground">{s.owner}</td>
          <td class="px-4 py-2.5 text-right"><Button variant="ghost" size="sm" onclick={() => openLogs(s)}><Terminal class="size-3.5" /> logs</Button></td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>

{#if sel}
  <div class="mt-4 rounded-lg border border-border bg-card">
    <div class="flex items-center gap-2 border-b border-border px-3 py-2">
      <Terminal class="size-3.5" /><span class="text-meta font-medium">{sel.name}</span><span class="text-micro text-muted-foreground">logs</span>
      <button onclick={() => (sel = null)} class="ml-auto text-muted-foreground hover:text-foreground"><X class="size-4" /></button>
    </div>
    {#if logsErr}<p class="p-3 text-meta text-destructive">{logsErr}</p>
    {:else}<pre class="max-h-96 overflow-auto p-3 font-mono text-micro leading-relaxed">{logs.join('\n')}</pre>{/if}
  </div>
{/if}
