<script>
  import { onMount } from 'svelte';
  import { Zap, ArrowRightLeft, Server, Users, Activity } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import Alert from '../components/Alert.svelte';
  import { fetchAPI } from '../utils/api.js';
  import { copyToClipboard } from '../utils/clipboard.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');

  async function fetchData() {
    try {
      data = await fetchAPI('/admin/api/models');
    } catch (e) {
      error = e.message || 'Failed to load the model catalog. The admin API may be unreachable; refresh the page to retry.';
    } finally {
      loading = false;
    }
  }

  onMount(() => { fetchData(); });

  // ── Derived: summary stats ──
  const modelSummary = $derived.by(() => {
    if (!data?.models) return null;
    const models = data.models;
    const available = models.filter(m => m.agent && m.agent !== 'Unbound').length;
    const unavailable = models.length - available;
    const providers = new Set(
      models.map(m => m.id.split('/')[0] || 'other')
    );
    return {
      total: models.length,
      available,
      unavailable,
      providers: providers.size
    };
  });

  // ── Derived: models grouped by provider ──
  const providerGroups = $derived.by(() => {
    if (!data?.models) return [];
    const map = new Map();
    for (const m of data.models) {
      const provider = m.id.split('/')[0] || 'other';
      if (!map.has(provider)) {
        map.set(provider, []);
      }
      map.get(provider).push(m);
    }
    // Sort providers alphabetically
    return [...map.entries()].sort(([a], [b]) => a.localeCompare(b));
  });
</script>

<div class="space-y-6 page-enter">
  <PageHeader title="Model Catalog & Registry" subtitle="Live registered models with dynamic fallback & transparent alias resolution">
    {#if data}
      <StatusBadge variant="teal">{data.count} registered models</StatusBadge>
      <StatusBadge variant="muted" mono>{data.agents} upstream agents</StatusBadge>
    {/if}
  </PageHeader>

  <!-- Fetch Error -->
  {#if error}
    <Alert variant="error" message={error} ondismiss={() => error = ''} />
  {/if}

  <!-- Model Health Summary -->
  {#if modelSummary}
    <div class="fp-card overflow-hidden">
      <div class="p-4 border-b border-[var(--fp-border)] flex items-center gap-2">
        <Activity size={18} class="text-[var(--fp-teal)]" />
        <h2 class="text-base font-semibold text-white">Model Health</h2>
      </div>
      <div class="p-4 grid grid-cols-2 sm:grid-cols-4 gap-4">
        <div>
          <span class="text-xs text-[var(--fp-muted)] uppercase tracking-wider">Total Models</span>
          <p class="text-2xl font-bold text-white mt-1">{modelSummary.total}</p>
        </div>
        <div>
          <span class="text-xs text-[var(--fp-muted)] uppercase tracking-wider">Available</span>
          <p class="text-2xl font-bold text-[var(--fp-teal)] mt-1">{modelSummary.available}</p>
        </div>
        <div>
          <span class="text-xs text-[var(--fp-muted)] uppercase tracking-wider">Unavailable</span>
          <p class="text-2xl font-bold text-[var(--fp-dim)] mt-1">{modelSummary.unavailable}</p>
        </div>
        <div>
          <span class="text-xs text-[var(--fp-muted)] uppercase tracking-wider">Providers</span>
          <p class="text-2xl font-bold text-[var(--fp-blue)] mt-1">{modelSummary.providers}</p>
        </div>
      </div>
      <div class="px-4 pb-3">
        <p class="text-[11px] text-[var(--fp-dim)]">Models synced from upstream every 6h</p>
      </div>
    </div>
  {/if}

  <!-- Models Grouped by Provider -->
  {#if data && providerGroups.length > 0}
    <div class="space-y-4">
      {#each providerGroups as [provider, models]}
        <div class="fp-card overflow-hidden">
          <div class="p-4 border-b border-[var(--fp-border)] flex items-center justify-between">
            <div class="flex items-center gap-2">
              <Server size={16} class="text-[var(--fp-amber)]" />
              <h3 class="text-sm font-semibold text-white capitalize">{provider}</h3>
              <span class="text-[10px] text-[var(--fp-dim)] bg-[var(--fp-surface)] px-1.5 py-0.5 rounded">{models.length} model{models.length !== 1 ? 's' : ''}</span>
            </div>
          </div>
          <div class="overflow-x-auto">
            <table class="fp-table">
              <thead>
                <tr>
                  <th scope="col" class="w-8"></th>
                  <th scope="col">Model Identifier</th>
                  <th scope="col">Upstream Agent</th>
                  <th scope="col" class="w-24 text-right">Copy</th>
                </tr>
              </thead>
              <tbody>
                {#each models as m}
                  {@const hasAgent = m.agent && m.agent !== 'Unbound'}
                  <tr>
                    <td class="text-center">
                      {#if hasAgent}
                        <span class="inline-block w-2 h-2 rounded-full bg-[var(--fp-teal)]" title="Available"></span>
                      {:else}
                        <span class="inline-block w-2 h-2 rounded-full bg-[var(--fp-dim)]" title="No agent bound"></span>
                      {/if}
                    </td>
                    <td>
                      <button
                        type="button"
                        onclick={() => copyToClipboard(m.id)}
                        class="font-bold text-white hover:text-[var(--fp-amber)] text-left transition-colors flex items-center gap-2"
                        title="Click to copy model ID"
                      >
                        <span>{m.id}</span>
                        {#if m.id.includes('deepseek-v4-flash')}
                          <span class="px-1.5 py-0.5 rounded bg-[var(--fp-amber)]/15 text-[var(--fp-amber)] border border-[var(--fp-amber)]/30 text-[10px] uppercase font-sans font-semibold">default</span>
                        {/if}
                      </button>
                    </td>
                    <td class="text-[var(--fp-muted)]">{m.agent || 'Unbound'}</td>
                    <td class="text-right">
                      <CopyButton text={m.id} variant="labeled" />
                    </td>
                  </tr>
                {/each}
              </tbody>
            </table>
          </div>
        </div>
      {/each}
    </div>
  {:else if data && !loading}
    <div class="fp-card overflow-hidden">
      <div class="p-6 text-center text-[var(--fp-dim)]">No models registered.</div>
    </div>
  {/if}

  <!-- Model Aliases -->
  {#if data?.has_aliases && data?.aliases?.length > 0}
    <div class="fp-card overflow-hidden">
      <div class="p-4 border-b border-[var(--fp-border)] flex items-center gap-2">
        <ArrowRightLeft size={18} class="text-[var(--fp-blue)]" />
        <div>
          <h2 class="text-base font-semibold text-white">Active Model Aliases</h2>
          <p class="text-xs text-[var(--fp-muted)] mt-0.5">Configured in <code class="text-[var(--fp-amber)]">MODEL_ALIASES</code>. Client requests are rewritten to their target models.</p>
        </div>
      </div>
      <div class="overflow-x-auto">
        <table class="fp-table">
          <thead>
            <tr>
              <th scope="col">Client Alias</th>
              <th scope="col">Target Model ID</th>
              <th scope="col" class="w-24 text-right">Action</th>
            </tr>
          </thead>
          <tbody>
            {#each data.aliases as a}
              <tr>
                <td class="font-bold text-[var(--fp-blue)]">{a.alias}</td>
                <td class="text-white">{a.real}</td>
                <td class="text-right">
                  <CopyButton text={a.alias} variant="labeled" />
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    </div>
  {/if}
</div>
