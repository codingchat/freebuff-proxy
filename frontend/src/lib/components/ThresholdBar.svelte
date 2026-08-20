<script>
  /**
   * ThresholdBar — usage/progress bar with threshold markers.
   *
   * @prop {number} value - current value (0-100)
   * @prop {number[]} [thresholds] - marker positions (default [50, 80])
   * @prop {string} [label] - left label
   * @prop {string} [suffix] - right suffix (e.g. "%")
   * @prop {'amber'|'teal'|'red'|'auto'} [color='auto'] - bar color
   */
  let { value = 0, thresholds = [50, 80], label = '', suffix = '%', color = 'auto' } = $props();

  let barColor = $derived(
    color !== 'auto' ? color :
    value >= 80 ? 'red' :
    value >= 50 ? 'amber' :
    'teal'
  );

  let barWidth = $derived(Math.min(value, 100));
</script>

<div class="space-y-1">
  <div class="relative w-full bg-[var(--fp-input-bg)] h-2 rounded-full overflow-hidden border border-[var(--fp-border)]">
    <!-- Threshold markers -->
    {#each thresholds as t}
      <div
        class="absolute top-0 h-full w-px bg-[var(--fp-dim)]/40"
        style="left: {t}%"
      ></div>
    {/each}
    <!-- Bar fill -->
    <div
      class="h-full transition-all duration-500 ease-out rounded-full
        {barColor === 'red' ? 'bg-[var(--fp-red)]' : ''}
        {barColor === 'amber' ? 'bg-[var(--fp-amber)]' : ''}
        {barColor === 'teal' ? 'bg-[var(--fp-teal)]' : ''}"
      style="width: {barWidth}%"
    ></div>
  </div>
  {#if label || suffix}
    <div class="flex justify-between text-[11px] text-[var(--fp-dim)] font-mono">
      <span>{label}</span>
      <span class="tabular-nums">{value}{suffix}</span>
    </div>
  {/if}
</div>
