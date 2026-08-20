<script>
  import { TrendingUp, TrendingDown, Minus } from '@lucide/svelte';

  /**
   * StatCard — metric display tile with label, value, optional sparkline and trend.
   *
   * @prop {string} label
   * @prop {string|number} value
   * @prop {string} [description]
   * @prop {string} [sparkHtml] - SVG sparkline HTML
   * @prop {'up'|'down'|'flat'|null} [trend] - trend direction
   * @prop {string} [trendLabel] - e.g. "+12% vs last hour"
   */
  let { label, value, description, sparkHtml, trend = null, trendLabel = '' } = $props();
</script>

<div class="fp-card p-5 flex flex-col justify-between">
  <div>
    <div class="flex items-center justify-between mb-1">
      <div class="text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider">{label}</div>
      {#if trend}
        <div class="flex items-center gap-1 text-xs
          {trend === 'up' ? 'text-[var(--fp-teal)]' : ''}
          {trend === 'down' ? 'text-[var(--fp-red)]' : ''}
          {trend === 'flat' ? 'text-[var(--fp-dim)]' : ''}">
          {#if trend === 'up'}
            <TrendingUp size={12} />
          {:else if trend === 'down'}
            <TrendingDown size={12} />
          {:else}
            <Minus size={12} />
          {/if}
          {#if trendLabel}
            <span class="font-mono">{trendLabel}</span>
          {/if}
        </div>
      {/if}
    </div>
    <div class="text-3xl font-bold text-white font-mono tabular-nums">{value}</div>
  </div>
  {#if sparkHtml}
    <div class="mt-4 overflow-hidden">
      {@html sparkHtml}
    </div>
  {/if}
  {#if description}
    <p class="text-xs text-[var(--fp-muted)] mt-4">{description}</p>
  {/if}
</div>
