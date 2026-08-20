<script>
  import { Radio, ExternalLink, Coins } from '@lucide/svelte';
  import Alert from '../components/Alert.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import StatCard from '../components/StatCard.svelte';
  import { fetchAPI } from '../utils/api.js';
  import { usePolling } from '../utils/polling.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');

  async function fetchMetrics() {
    try {
      data = await fetchAPI('/admin/api/metrics');
      error = '';
    } catch (e) {
      const detail = e?.message || '';
      error = detail ? `Could not load metrics: ${detail}. Retrying every 5 seconds.` : 'Could not load metrics. Retrying every 5 seconds.';
    } finally {
      loading = false;
    }
  }

  usePolling(fetchMetrics, 5000);

  function trendLabel(trend) {
    if (!trend || trend.direction === 'flat') return '';
    const sign = trend.percentage > 0 ? '+' : '';
    return `${sign}${Math.round(trend.percentage)}% vs last 50s`;
  }

  const promConfigText = `# Prometheus scrape config for freebuff-proxy
scrape_configs:
  - job_name: "freebuff-proxy"
    scrape_interval: 15s
    static_configs:
      - targets: ["localhost:3210"]
        labels:
          instance: "freebuff-proxy"`;
</script>

<div class="space-y-6 page-enter">
  <PageHeader title="Telemetry & Metrics" subtitle="Live metrics counters and trends sampled every 5s">
    {#if data}
      <StatusBadge variant="muted" mono>{data.models} models</StatusBadge>
      <StatusBadge variant="muted" mono>{data.sample_count} samples cached</StatusBadge>
    {/if}
  </PageHeader>

  <!-- Error State -->
  {#if error}
    <Alert variant="error" message={error} dismissable={false} />
  {/if}

  <!-- Loading Skeleton -->
  {#if loading}
    <div class="grid grid-cols-1 md:grid-cols-3 gap-5">
      {#each [1,2,3] as _}
        <div class="skeleton skeleton-card"></div>
      {/each}
    </div>
  {/if}

  <!-- Metric Cards -->
  {#if data}
    <div class="grid grid-cols-1 md:grid-cols-3 gap-5">
      <StatCard
        label="Requests Served"
        value={data.requests_total}
        sparkHtml={data.requests_spark}
        trend={data.requests_trend?.direction}
        trendLabel={trendLabel(data.requests_trend)}
        description="Sampled every 5s, max 120 samples"
      />
      <StatCard
        label="Transient Retries"
        value={data.transient_retries}
        sparkHtml={data.retries_spark}
        trend={data.retries_trend?.direction}
        trendLabel={trendLabel(data.retries_trend)}
        description="Sampled every 5s, max 120 samples"
      />
      <StatCard
        label="Fingerprint Rotations"
        value={data.fingerprint_rotations}
        description="Stealth browser TLS profile rotation events"
      />
    </div>
  {/if}

  <!-- Per-Token Metrics -->
  {#if data && data.per_tokens?.length > 0}
    <div class="fp-card p-5 space-y-4">
      <div class="flex items-center gap-2">
        <Coins size={18} class="text-[var(--fp-teal)]" />
        <h2 class="text-base font-semibold text-white">Per-Token Breakdown</h2>
      </div>
      <div class="overflow-x-auto">
        <table class="w-full text-xs">
          <thead>
            <tr class="text-[var(--fp-muted)] border-b border-[var(--fp-border)]">
              <th class="text-left py-2 pr-3 font-semibold uppercase tracking-wider">Token</th>
              <th class="text-right py-2 px-3 font-semibold uppercase tracking-wider">Requests</th>
              <th class="text-right py-2 px-3 font-semibold uppercase tracking-wider">Retries</th>
              <th class="text-right py-2 px-3 font-semibold uppercase tracking-wider">Rotations</th>
              <th class="text-right py-2 px-3 font-semibold uppercase tracking-wider">Spend (Day)</th>
              <th class="text-right py-2 pl-3 font-semibold uppercase tracking-wider">Risk</th>
            </tr>
          </thead>
          <tbody>
            {#each data.per_tokens as tok}
              <tr class="border-b border-[var(--fp-border)]/40 hover:bg-[var(--fp-surface)]/30 transition-colors">
                <td class="py-2.5 pr-3 font-mono text-white">#{tok.token}</td>
                <td class="py-2.5 px-3 text-right font-mono tabular-nums text-white">{tok.requests_24h}</td>
                <td class="py-2.5 px-3 text-right font-mono tabular-nums {tok.transient_retries > 0 ? 'text-[var(--fp-amber)]' : 'text-[var(--fp-muted)]'}">{tok.transient_retries}</td>
                <td class="py-2.5 px-3 text-right font-mono tabular-nums {tok.fingerprint_rotations > 0 ? 'text-[var(--fp-amber)]' : 'text-[var(--fp-muted)]'}">{tok.fingerprint_rotations}</td>
                <td class="py-2.5 px-3 text-right font-mono tabular-nums text-white">{tok.spend_day}</td>
                <td class="py-2.5 pl-3 text-right">
                  {#if tok.risk_level === 'high' || tok.risk_level === 'critical'}
                    <span class="text-[var(--fp-red)] uppercase font-semibold">{tok.risk_level}</span>
                  {:else if tok.risk_level === 'moderate'}
                    <span class="text-[var(--fp-amber)] uppercase font-semibold">{tok.risk_level}</span>
                  {:else}
                    <span class="text-[var(--fp-teal)] uppercase font-semibold">{tok.risk_level || 'low'}</span>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
      <p class="text-xs text-[var(--fp-muted)]">Per-token breakdown of usage across all configured API keys.</p>
    </div>
  {/if}

  <!-- Prometheus Card -->
  <div class="fp-card p-5 space-y-3">
    <div class="flex items-center gap-2">
      <Radio size={18} class="text-[var(--fp-amber)]" />
      <h2 class="text-base font-semibold text-white">Prometheus Exporter Feed</h2>
    </div>
    <p class="text-xs text-[var(--fp-muted)]">
      Real-time Prometheus format metrics are exposed at <code class="text-[var(--fp-amber)] font-mono">/metrics</code> for Grafana, Prometheus scrapers, Datadog, or SigNoz. Includes per-token counters, cooldown locks, session status, and microsecond phase latencies.
    </p>
    <div class="flex flex-wrap items-center gap-3">
      <a
        href="/metrics"
        target="_blank"
        rel="noopener noreferrer"
        class="fp-btn-secondary"
      >
        <ExternalLink size={14} />
        <span>Open Raw /metrics Feed</span>
      </a>
      <CopyButton text={promConfigText} variant="labeled" label="Copy Prometheus Config" />
    </div>
    <p class="text-xs text-[var(--fp-muted)]">Real-time Prometheus format for Grafana, Datadog, and other monitoring backends.</p>
  </div>
</div>
