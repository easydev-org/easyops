<script lang="ts">
  // easyops cluster console. Pages: Overview / Services / Sandboxes / PVCs /
  // Helm. Same-origin; calls ops.v1 Connect (JSON) with the service token.
  import { Server, LayoutDashboard, Container, Boxes, HardDrive, Package } from '@lucide/svelte'
  import Button from '$lib/components/ui/button.svelte'
  import Input from '$lib/components/ui/input.svelte'
  import { Api } from '$lib/api'
  import Overview from '$lib/pages/Overview.svelte'
  import Services from '$lib/pages/Services.svelte'
  import Sandboxes from '$lib/pages/Sandboxes.svelte'
  import PVCs from '$lib/pages/PVCs.svelte'
  import Helm from '$lib/pages/Helm.svelte'

  const KEY = 'easyopsToken'
  let token = $state(localStorage.getItem(KEY) ?? '')
  let page = $state<'overview' | 'services' | 'sandboxes' | 'pvcs' | 'helm'>('overview')
  const api = new Api(() => token)
  function save() {
    localStorage.setItem(KEY, token.trim())
    const p = page
    page = 'overview'
    queueMicrotask(() => (page = p))
  }
  const nav = [
    ['overview', LayoutDashboard],
    ['services', Container],
    ['sandboxes', Boxes],
    ['pvcs', HardDrive],
    ['helm', Package],
  ] as const
</script>

<div class="flex h-dvh flex-col bg-background text-foreground">
  <header class="flex h-12 shrink-0 items-center gap-3 border-b border-border bg-background/80 px-5 backdrop-blur">
    <div class="flex size-7 items-center justify-center rounded-md bg-primary text-primary-foreground"><Server class="size-4" /></div>
    <div class="leading-tight"><h1 class="text-body font-semibold">easyops</h1><p class="text-micro text-muted-foreground">cluster</p></div>
    <div class="ml-auto flex items-center gap-2">
      <Input type="password" bind:value={token} placeholder="service token" class="w-64" />
      <Button onclick={save}>Connect</Button>
    </div>
  </header>
  <nav class="flex shrink-0 gap-1 border-b border-border px-3">
    {#each nav as [id, Icon] (id)}
      <button onclick={() => (page = id)}
        class="flex items-center gap-1.5 border-b-2 px-3 py-2 text-meta transition {page === id ? 'border-primary text-foreground' : 'border-transparent text-muted-foreground hover:text-foreground'}">
        <Icon class="size-3.5" />{id}
      </button>
    {/each}
  </nav>
  <main class="min-h-0 flex-1 overflow-auto px-5 py-5">
    {#key page + token}
      {#if page === 'overview'}<Overview {api} />
      {:else if page === 'services'}<Services {api} />
      {:else if page === 'sandboxes'}<Sandboxes {api} />
      {:else if page === 'pvcs'}<PVCs {api} />
      {:else}<Helm {api} />{/if}
    {/key}
  </main>
</div>
