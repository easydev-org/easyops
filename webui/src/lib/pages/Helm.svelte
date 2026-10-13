<script lang="ts">
  import { onMount } from 'svelte'
  import { Package, X, Layers } from '@lucide/svelte'
  import Button from '$lib/components/ui/button.svelte'
  import Badge from '$lib/components/ui/badge.svelte'
  import type { Api, Helm as HelmRel, HelmObject } from '$lib/api'
  import { badgeVariant, fmtWhen } from '$lib/format'

  let { api }: { api: Api } = $props()
  let items = $state<HelmRel[]>([])
  let err = $state('')
  let sel = $state<HelmRel | null>(null)
  let objects = $state<HelmObject[]>([])
  let history = $state<{ revision?: number; status?: string; chartVersion?: string; appVersion?: string; updatedAt?: string }[]>([])
  let dErr = $state('')

  async function load() {
    err = ''
    try {
      items = (await api.helm()).releases ?? []
    } catch (e) {
      err = String(e)
    }
  }
  onMount(load)
  async function open(r: HelmRel) {
    sel = r
    objects = []
    history = []
    dErr = ''
    try {
      const [o, h] = await Promise.all([api.helmObjects(r.owner ?? '', r.name), api.helmHistory(r.owner ?? '', r.name)])
      objects = o.objects ?? []
      history = h.revisions ?? []
    } catch (e) {
      dErr = String(e)
    }
  }
</script>
{#if err}<p class="mb-3 text-meta text-destructive">{err}</p>{/if}
<div class="mb-3 flex items-center gap-2"><h3 class="text-body font-semibold">Helm releases</h3><Button variant="outline" size="sm" onclick={load}>refresh</Button></div>
<div class="overflow-hidden rounded-lg border border-border bg-card">
  <table class="w-full border-collapse">
    <thead><tr class="border-b border-border text-left text-micro uppercase tracking-wider text-muted-foreground">
      <th class="px-4 py-3 font-medium">release</th><th class="px-4 py-3 font-medium">ns</th><th class="px-4 py-3 font-medium">rev</th>
      <th class="px-4 py-3 font-medium">status</th><th class="px-4 py-3 font-medium">chart</th><th class="px-4 py-3 font-medium">updated</th><th class="px-4 py-3"></th>
    </tr></thead>
    <tbody>
      {#each items as r (r.name)}
        <tr class="border-b border-border/60 last:border-0 hover:bg-secondary">
          <td class="px-4 py-2.5 text-meta font-medium">{r.name}</td>
          <td class="px-4 py-2.5 text-meta text-muted-foreground">{r.namespace}</td>
          <td class="px-4 py-2.5 text-meta tabular-nums">{r.revision}</td>
          <td class="px-4 py-2.5"><Badge variant={badgeVariant(r.status)}>{r.status}</Badge>{#if r.slot}<span class="ml-2 text-micro text-muted-foreground">{r.slot}{r.activeSlot ? ` (active ${r.activeSlot})` : ''}</span>{/if}</td>
          <td class="px-4 py-2.5 text-meta text-muted-foreground">{r.chartVersion}{r.appVersion ? ` / ${r.appVersion}` : ''}</td>
          <td class="px-4 py-2.5 text-micro text-muted-foreground">{fmtWhen(r.updatedAt)}</td>
          <td class="px-4 py-2.5 text-right"><Button variant="ghost" size="sm" onclick={() => open(r)}><Layers class="size-3.5" /> detail</Button></td>
        </tr>
      {/each}
    </tbody>
  </table>
</div>

{#if sel}
  <div class="mt-4 rounded-lg border border-border bg-card">
    <div class="flex items-center gap-2 border-b border-border px-3 py-2">
      <Package class="size-3.5" /><span class="text-meta font-medium">{sel.name}</span><span class="text-micro text-muted-foreground">objects + history</span>
      <button onclick={() => (sel = null)} class="ml-auto text-muted-foreground hover:text-foreground"><X class="size-4" /></button>
    </div>
    {#if dErr}<p class="p-3 text-meta text-destructive">{dErr}</p>
    {:else}
      <div class="grid gap-4 p-3 lg:grid-cols-2">
        <div>
          <p class="mb-1 text-micro uppercase tracking-wider text-muted-foreground">objects</p>
          <table class="w-full border-collapse"><tbody>
            {#each objects as o ((o.kind ?? '') + '/' + (o.name ?? ''))}
              <tr class="border-b border-border/60 last:border-0">
                <td class="py-1.5 text-meta">{o.kind}/{o.name}</td>
                <td class="py-1.5 text-right"><Badge variant={o.ready ? 'success' : badgeVariant(o.phase)}>{o.ready ? 'ready' : o.phase ?? '—'}</Badge></td>
              </tr>
            {/each}
          </tbody></table>
        </div>
        <div>
          <p class="mb-1 text-micro uppercase tracking-wider text-muted-foreground">history</p>
          <table class="w-full border-collapse"><tbody>
            {#each history as h (h.revision)}
              <tr class="border-b border-border/60 last:border-0">
                <td class="py-1.5 text-meta tabular-nums">rev {h.revision}</td>
                <td class="py-1.5 text-meta text-muted-foreground">{h.chartVersion}</td>
                <td class="py-1.5 text-right"><Badge variant={badgeVariant(h.status)}>{h.status}</Badge></td>
              </tr>
            {/each}
          </tbody></table>
        </div>
      </div>
    {/if}
  </div>
{/if}
