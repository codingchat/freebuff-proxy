<script>
  import { onMount } from 'svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import Stat from '../components/Stat.svelte';
  import Card from '../components/Card.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import Alert from '../components/Alert.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import Button from '../components/Button.svelte';
  import { fetchAPI } from '../api/client.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');

  async function load() {
    loading = true;
    error = '';
    try {
      data = await fetchAPI('/admin/api/models');
    } catch (e) {
      error = e.message || 'Failed to load the model catalog.';
    } finally {
      loading = false;
    }
  }

  onMount(load);

  const servedCount = $derived(
    data ? data.models.filter((m) => m.agent).length : 0
  );

  // Client aliases (MODEL_ALIASES) that resolve to a given model id.
  function aliasesFor(id) {
    if (!data?.aliases) return '';
    return data.aliases
      .filter((a) => a.real === id)
      .map((a) => a.alias)
      .join(', ');
  }
</script>

<div class="space-y-6 page-enter">
  <PageHeader title="Models" description="Served model catalog with upstream agent bindings and client aliases." />

  {#if error}
    <Alert tone="error" title="Failed to load models">
      <p>{error}</p>
      <div class="mt-3">
        <Button variant="secondary" size="sm" onclick={load}>Retry</Button>
      </div>
    </Alert>
  {:else if loading}
    <div role="status" aria-label="Loading model catalog" class="space-y-6">
      <div class="skeleton skeleton-text w-56"></div>
      <div class="fp-card">
        <div class="p-4 space-y-3">
          <div class="skeleton skeleton-line w-40"></div>
          {#each Array(6) as _}
            <div class="skeleton skeleton-line w-full"></div>
          {/each}
        </div>
      </div>
    </div>
  {:else if data}
    <Stat
      label="Served Models"
      value={servedCount}
      hint={`${data.count} registered · ${data.agents} agents`}
      tone={servedCount > 0 ? 'good' : 'idle'}
      big
    />

    {#if data.models.length > 0}
      <Card title="Model Catalog" pad="none">
        <div class="overflow-x-auto">
          <table class="fp-table">
            <thead>
              <tr>
                <th scope="col">Model ID</th>
                <th scope="col">Served</th>
                <th scope="col">Agent</th>
                <th scope="col">Client Aliases</th>
              </tr>
            </thead>
            <tbody>
              {#each data.models as m}
                {@const bound = Boolean(m.agent)}
                {@const aliases = aliasesFor(m.id)}
                <tr>
                  <td><span class="fp-num">{m.id}</span></td>
                  <td>
                    <StatusBadge
                      status={bound ? 'served' : 'unbound'}
                      tone={bound ? 'good' : 'idle'}
                    />
                  </td>
                  <td>
                    {#if bound}
                      <span class="fp-mono text-[var(--fp-muted)]">{m.agent}</span>
                    {:else}
                      <span class="text-[var(--fp-dim)]">—</span>
                    {/if}
                  </td>
                  <td>
                    {#if aliases}
                      <span class="fp-mono text-[var(--fp-muted)]">{aliases}</span>
                    {:else}
                      <span class="text-[var(--fp-dim)]">—</span>
                    {/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </Card>
    {:else}
      <EmptyState
        title="No models registered"
        description="The model registry is empty. Add model-to-agent mappings in the gateway config and reload."
      />
    {/if}
  {/if}
</div>
