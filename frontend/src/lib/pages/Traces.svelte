<script>
  import { Timer, ChevronDown } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import StatCard from '../components/StatCard.svelte';
  import PhaseTimeline from '../components/PhaseTimeline.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import Pagination from '../components/Pagination.svelte';
  import Alert from '../components/Alert.svelte';
  import { fetchAPI } from '../utils/api.js';
  import { usePolling } from '../utils/polling.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');
  let page = $state(0);
  let expandedTrace = $state(null);
  const PAGE_SIZE = 50;

  let pagedTraces = $derived.by(() => {
    const traces = data?.traces || [];
    const start = page * PAGE_SIZE;
    return traces.slice(start, start + PAGE_SIZE);
  });

  let totalPages = $derived.by(() => Math.ceil((data?.traces?.length || 0) / PAGE_SIZE));

  let traceSummary = $derived.by(() => {
    const traces = data?.traces || [];
    if (traces.length === 0) {
      return { total: 0, avgMs: 0, p95Ms: 0, successRate: 0 };
    }
    const durations = traces
      .map((t) => parseInt(t.ms, 10))
      .filter((n) => !isNaN(n))
      .sort((a, b) => a - b);
    const successCount = traces.filter((t) => t.status === 'ok').length;
    const total = traces.length;
    const avgMs = durations.length > 0
      ? Math.round(durations.reduce((s, v) => s + v, 0) / durations.length)
      : 0;
    const p95Index = Math.ceil(durations.length * 0.95) - 1;
    const p95Ms = durations.length > 0 ? durations[Math.max(0, p95Index)] : 0;
    const successRate = total > 0 ? Math.round((successCount / total) * 100) : 0;
    return { total, avgMs, p95Ms, successRate };
  });

  let successRateBadge = $derived.by(() => {
    const rate = traceSummary.successRate;
    if (rate > 95) return { variant: 'teal', label: `${rate}%` };
    if (rate > 80) return { variant: 'amber', label: `${rate}%` };
    return { variant: 'red', label: `${rate}%` };
  });

  async function fetchData() {
    try {
      data = await fetchAPI('/admin/api/traces');
      error = '';
      const pages = Math.ceil((data?.traces?.length || 0) / PAGE_SIZE);
      if (page > pages - 1) page = 0;
    } catch (e) {
      error = e.message || 'Failed to fetch traces';
    } finally {
      loading = false;
    }
  }

  usePolling(fetchData, 3000);

  function statusVariant(status) {
    if (status === 'ok') return 'teal';
    if (status === 'rate_limited') return 'amber';
    return 'red';
  }

  function toggleExpand(idx) {
    expandedTrace = expandedTrace === idx ? null : idx;
  }
</script>

<div class="space-y-6 page-enter">
  <PageHeader title="Live Request Traces" subtitle="Real-time routing decisions, duration metrics, and error classification">
    {#if data}
      <StatusBadge variant={data.enabled ? 'teal' : 'red'}>
        {data.enabled ? 'Ring Active (200 records)' : 'Ring Disabled'}
      </StatusBadge>
    {/if}
  </PageHeader>

  {#if error}
    <Alert variant="error" message={error} dismissable={false} />
  {:else if !data?.enabled}
    <EmptyState icon={Timer} title="Trace Viewer Disabled" description="The trace ring was not initialized (server started without dashboard log handler)." />
  {:else if !data?.traces || data.traces.length === 0}
    <EmptyState icon={Timer} title="No Chat Completions Yet" description="Incoming chat completion requests will appear here automatically." />
  {:else}
    <!-- Trace Summary Row -->
    <div class="grid grid-cols-2 md:grid-cols-4 gap-4">
      <StatCard
        label="Success Rate"
        value={successRateBadge.label}
        description="Request success rate"
      />
      <StatCard
        label="Total Traces"
        value={traceSummary.total.toLocaleString()}
        description="Requests in the trace ring"
      />
      <StatCard
        label="Avg Latency"
        value={`${traceSummary.avgMs}ms`}
        description="Mean request duration"
      />
      <StatCard
        label="P95 Latency"
        value={`${traceSummary.p95Ms}ms`}
        description="95th percentile duration"
      />
    </div>

    <!-- Trace Table -->
    <div class="fp-card overflow-hidden">
      <div class="overflow-x-auto">
        <table class="fp-table">
          <thead>
            <tr>
              <th scope="col"></th>
              <th scope="col">Time</th>
              <th scope="col">Token</th>
              <th scope="col">Model</th>
              <th scope="col">Status</th>
              <th scope="col">Duration</th>
              <th scope="col">Details / Error</th>
            </tr>
          </thead>
          <tbody>
            {#each pagedTraces as t, idx}
              <tr
                class="cursor-pointer hover:bg-[var(--fp-surface)] transition-colors {t.error ? 'bg-[var(--fp-red)]/5' : ''}"
                onclick={() => toggleExpand(idx)}
                role="button"
                tabindex="0"
                onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleExpand(idx); } }}
              >
                <td class="w-8 text-center">
                  <span class="inline-block transition-transform duration-200 {expandedTrace === idx ? 'rotate-180' : ''}">
                    <ChevronDown class="w-4 h-4 text-[var(--fp-muted)]" />
                  </span>
                </td>
                <td class="text-[var(--fp-muted)] whitespace-nowrap">{t.time}</td>
                <td class="font-bold text-white whitespace-nowrap">{t.token}</td>
                <td class="text-[var(--fp-muted)] whitespace-nowrap">{t.model}</td>
                <td class="whitespace-nowrap">
                  <StatusBadge variant={statusVariant(t.status)}>{t.status}</StatusBadge>
                </td>
                <td class="text-white font-semibold whitespace-nowrap tabular-nums">{t.ms}</td>
                <td class="text-[var(--fp-red)] break-all">{t.error || '-'}</td>
              </tr>
              {#if expandedTrace === idx}
                <tr>
                  <td colspan="7" class="px-4 py-3 bg-[var(--fp-input-bg)]/30">
                    {#if t.phases && t.phases.length > 0}
                      <PhaseTimeline phases={t.phases} totalMs={parseInt(t.ms, 10)} />
                    {:else}
                      <span class="text-[var(--fp-dim)] text-xs">No phase timing data recorded for this trace.</span>
                    {/if}
                  </td>
                </tr>
              {/if}
            {/each}
          </tbody>
        </table>
      </div>
      <Pagination
        {page}
        totalPages={totalPages}
        totalItems={data.traces.length}
        itemLabel="traces"
        onchange={(p) => page = p}
      />
    </div>
  {/if}
</div>
