<script>
  import { ListFilter, Search, RefreshCw, AlertCircle, AlertTriangle, CheckCircle2, Info, X, Download, ChevronDown, ChevronRight } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import EmptyState from '../components/EmptyState.svelte';
  import Pagination from '../components/Pagination.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import { fetchAPI } from '../utils/api.js';
  import { usePolling } from '../utils/polling.js';
  import { formatTime, parseLogFields } from '../utils/format.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');
  let filterLevel = $state('');
  let filterMsg = $state('');
  let page = $state(0);
  let expandedLog = $state(null);
  const PAGE_SIZE = 50;

  let pagedEntries = $derived.by(() => {
    const entries = data?.entries || [];
    const start = page * PAGE_SIZE;
    return entries.slice(start, start + PAGE_SIZE);
  });

  let totalPages = $derived.by(() => Math.ceil((data?.entries?.length || 0) / PAGE_SIZE));

  let logSummary = $derived.by(() => {
    const entries = data?.entries || [];
    const counts = { error: 0, warn: 0, info: 0, debug: 0 };
    for (const e of entries) {
      if (e.level in counts) counts[e.level]++;
    }
    return counts;
  });

  let errorRate = $derived.by(() => {
    const total = data?.entries?.length || 0;
    if (total === 0) return '0';
    return ((logSummary.error / total) * 100).toFixed(1);
  });

  let hasActiveFilter = $derived.by(() => filterLevel !== '' || filterMsg.trim() !== '');

  function exportLogs() {
    const entries = data?.entries || [];
    const blob = new Blob([JSON.stringify(entries, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    const ts = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
    a.download = `freebuff-logs-${ts}.json`;
    a.click();
    URL.revokeObjectURL(url);
  }

  function toggleExpand(idx) {
    expandedLog = expandedLog === idx ? null : idx;
  }

  async function fetchLogs() {
    try {
      const query = new URLSearchParams();
      if (filterLevel) query.set('level', filterLevel);
      if (filterMsg.trim()) query.set('msg', filterMsg.trim());
      data = await fetchAPI(`/admin/api/logs?${query.toString()}`);
      error = '';
      const tp = Math.ceil((data?.entries?.length || 0) / PAGE_SIZE);
      if (page > tp - 1) page = 0;
    } catch (e) {
      error = e.message
        ? `Could not load log entries: ${e.message}. Polling retries automatically.`
        : 'Could not load log entries. Polling retries automatically.';
    } finally {
      loading = false;
    }
  }

  function handleFilterChange() { page = 0; expandedLog = null; fetchLogs(); }

  usePolling(fetchLogs, 3000);

  function levelColor(level) {
    switch(level) {
      case 'error': return 'red';
      case 'warn': return 'amber';
      case 'info': return 'teal';
      default: return 'blue';
    }
  }

  function levelIcon(level) {
    switch(level) {
      case 'error': return AlertCircle;
      case 'warn': return AlertTriangle;
      case 'info': return CheckCircle2;
      default: return Info;
    }
  }
</script>

<div class="space-y-6 page-enter">
  <PageHeader title="In-Memory Log Stream" subtitle="Structured log entries from the ring buffer (500 max) with live level filtering & search — updates every 3s">
    {#if data}
      <StatusBadge variant={data.enabled ? 'teal' : 'red'}>
        {#if data.enabled}
          <RefreshCw size={12} class="animate-spin" />
          Live · 3s
        {:else}
          Disabled
        {/if}
      </StatusBadge>
      <StatusBadge variant="muted" mono>{data.entries?.length || 0}</StatusBadge>
    {/if}
  </PageHeader>

  {#if data?.enabled}
    <!-- Summary Row -->
    <div class="fp-card p-4 flex flex-col sm:flex-row items-center gap-4">
      <div class="flex items-center gap-3 flex-wrap">
        <StatusBadge variant="red" mono>
          <AlertCircle size={11} />
          {logSummary.error} error
        </StatusBadge>
        <StatusBadge variant="amber" mono>
          <AlertTriangle size={11} />
          {logSummary.warn} warn
        </StatusBadge>
        <StatusBadge variant="teal" mono>
          <CheckCircle2 size={11} />
          {logSummary.info} info
        </StatusBadge>
        <span class="text-xs text-[var(--fp-dim)] font-mono">
          {data.entries?.length || 0} total · {errorRate}% errors
        </span>
      </div>
      <div class="flex items-center gap-2 sm:ml-auto">
        <button
          type="button"
          onclick={() => { filterLevel = ''; filterMsg = ''; handleFilterChange(); }}
          disabled={!hasActiveFilter}
          class="px-3 py-1.5 min-h-10 rounded-lg text-xs font-medium border transition-colors
            {hasActiveFilter
              ? 'bg-[var(--fp-surface-3)] hover:bg-[var(--fp-border-bright)] border-[var(--fp-border-bright)] text-white'
              : 'bg-[var(--fp-surface-2)] border-[var(--fp-border)] text-[var(--fp-dim)] opacity-50 cursor-not-allowed'}"
        >
          Clear Filters
        </button>
        <button
          type="button"
          onclick={exportLogs}
          disabled={!data?.entries?.length}
          class="px-3 py-1.5 min-h-10 rounded-lg text-xs font-medium border transition-colors
            {data?.entries?.length
              ? 'bg-[var(--fp-surface-3)] hover:bg-[var(--fp-border-bright)] border-[var(--fp-border-bright)] text-white'
              : 'bg-[var(--fp-surface-2)] border-[var(--fp-border)] text-[var(--fp-dim)] opacity-50 cursor-not-allowed'}"
        >
          <Download size={12} class="inline-block mr-1.5 -mt-0.5" />
          Export JSON
        </button>
      </div>
    </div>

    <!-- Filters -->
    <div class="fp-card p-4 flex flex-col sm:flex-row items-center gap-3">
      {#if hasActiveFilter}
        <div class="flex items-center gap-1.5 px-2.5 py-1 rounded-full bg-[var(--fp-amber)]/10 border border-[var(--fp-amber)]/30 text-[11px] font-medium text-[var(--fp-amber)] shrink-0">
          Filtering: {filterLevel ? `level=${filterLevel}` : ''}{filterLevel && filterMsg.trim() ? ', ' : ''}{filterMsg.trim() ? `msg="${filterMsg.trim()}"` : ''}
        </div>
      {/if}
      <div class="w-full sm:w-48">
        <label for="log-level-select" class="sr-only">Level</label>
        <select
          id="log-level-select"
          bind:value={filterLevel}
          onchange={handleFilterChange}
          class="fp-input fp-input-mono text-xs"
        >
          <option value="">ALL LEVELS</option>
          <option value="info">INFO</option>
          <option value="debug">DEBUG</option>
          <option value="warn">WARN</option>
          <option value="error">ERROR</option>
        </select>
      </div>
      <div class="w-full flex-1 relative flex items-center">
        <label for="log-search-input" class="sr-only">Search</label>
        <Search size={14} class="absolute left-3 text-[var(--fp-dim)] pointer-events-none" />
        <input
          id="log-search-input"
          type="text"
          bind:value={filterMsg}
          oninput={handleFilterChange}
          placeholder="Filter by message, req_id, path..."
          class="fp-input text-xs pl-9 pr-8 py-1.5 h-8.5"
        />
        {#if filterMsg}
          <button
            type="button"
            onclick={() => { filterMsg = ''; handleFilterChange(); }}
            class="absolute right-2 p-1 rounded hover:bg-[var(--fp-surface-3)] text-[var(--fp-dim)] hover:text-white transition-colors"
            aria-label="Clear search filter"
          >
            <X size={13} />
          </button>
        {/if}
      </div>
    </div>

    <!-- Entries -->
    {#if !data?.entries || data.entries.length === 0}
      <EmptyState icon={ListFilter} title="No Matching Log Records" description="No log entries matched your filter criteria." />
    {:else}
      <div class="space-y-1.5">
        {#each pagedEntries as e, idx}
          {@const LevelIcon = levelIcon(e.level)}
          {@const fields = parseLogFields(e.fields)}
          {@const isExpanded = expandedLog === idx}
          {@const entryJson = JSON.stringify({ time: e.time, level: e.level, message: e.message, fields: e.fields || '' }, null, 2)}
          <div class="fp-card overflow-hidden">
            <!-- Row header: clickable to expand -->
            <button
              type="button"
              onclick={() => toggleExpand(idx)}
              class="w-full flex items-center gap-3 px-4 py-2.5 text-left hover:bg-[var(--fp-surface-2)]/60 transition-colors cursor-pointer"
              aria-expanded={isExpanded}
              aria-label="Toggle log entry detail"
            >
              {#if isExpanded}
                <ChevronDown size={14} class="text-[var(--fp-dim)] shrink-0" />
              {:else}
                <ChevronRight size={14} class="text-[var(--fp-dim)] shrink-0" />
              {/if}
              <StatusBadge variant={levelColor(e.level)}>
                <LevelIcon size={10} />
                {e.level}
              </StatusBadge>
              <span class="text-sm font-mono text-white font-medium flex-1 truncate">{e.message}</span>
              <span class="shrink-0 text-[11px] font-mono text-[var(--fp-dim)] tabular-nums">{formatTime(e.time)}</span>
              <!-- Stop click from toggling expand -->
              <span class="shrink-0" role="presentation" onclick={(e) => e.stopPropagation()}>
                <CopyButton text={entryJson} size={12} />
              </span>
            </button>

            <!-- Inline fields (always visible) -->
            {#if fields.length > 0}
              <div class="px-4 pb-2.5 pt-0">
                <div class="flex flex-wrap gap-1.5">
                  {#each fields as f}
                    <span class="inline-flex items-center gap-1.5 px-2 py-0.5 rounded fp-inset text-[10px] font-mono">
                      <span class="text-[var(--fp-dim)] font-semibold">{f.key}</span>
                      <span class="text-[var(--fp-muted)]">{f.value}</span>
                    </span>
                  {/each}
                </div>
              </div>
            {/if}

            <!-- Expanded: full JSON detail -->
            {#if isExpanded}
              <div class="px-4 pb-3 pt-0 border-t border-[var(--fp-border)]">
                <div class="flex items-center justify-between mt-2 mb-1.5">
                  <span class="text-[10px] font-semibold uppercase tracking-wider text-[var(--fp-dim)]">Full Entry</span>
                  <CopyButton text={entryJson} variant="labeled" label="Copy JSON" size={12} />
                </div>
                <pre class="p-3 rounded-lg bg-[var(--fp-input-bg)] text-[11px] font-mono text-[var(--fp-muted)] overflow-x-auto max-h-64 whitespace-pre-wrap">{entryJson}</pre>
              </div>
            {/if}
          </div>
        {/each}
      </div>
      <Pagination
        {page}
        totalPages={totalPages}
        totalItems={data.entries.length}
        itemLabel="entries"
        onchange={(p) => page = p}
      />
    {/if}
  {:else}
    <EmptyState icon={ListFilter} title="Log Ring Disabled" description="The server was started without an active logring handler." />
  {/if}
</div>
