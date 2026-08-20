<script>
  /**
   * PhaseTimeline — horizontal timing breakdown visualization.
   *
   * @prop {Array<{name: string, ms: number, color?: string}>} phases
   * @prop {number} [totalMs] - total for percentage calc (defaults to sum)
   */
  let { phases = [], totalMs = 0 } = $props();

  let computedTotal = $derived(totalMs || phases.reduce((s, p) => s + p.ms, 0));

  let phaseColors = [
    'bg-[var(--fp-teal)]',
    'bg-[#60A5FA]',
    'bg-[var(--fp-amber)]',
    'bg-[#AC94FA]',
    'bg-[#F97316]',
  ];
</script>

{#if phases.length > 0}
  <div class="space-y-2">
    <!-- Timeline bar -->
    <div class="flex h-3 rounded-full overflow-hidden bg-[var(--fp-input-bg)] border border-[var(--fp-border)]">
      {#each phases as phase, i}
        {#if phase.ms > 0}
          <div
            class="h-full transition-all duration-300 {phase.color || phaseColors[i % phaseColors.length]}
              {i === 0 ? 'rounded-l-full' : ''}
              {i === phases.length - 1 ? 'rounded-r-full' : ''}"
            style="width: {computedTotal > 0 ? (phase.ms / computedTotal * 100) : 0}%"
            title="{phase.name}: {phase.ms}ms"
          ></div>
        {/if}
      {/each}
    </div>
    <!-- Phase labels -->
    <div class="flex flex-wrap gap-x-4 gap-y-1">
      {#each phases as phase, i}
        <div class="flex items-center gap-1.5 text-xs">
          <span class="w-2 h-2 rounded-full {phase.color || phaseColors[i % phaseColors.length]}"></span>
          <span class="text-[var(--fp-muted)]">{phase.name}</span>
          <span class="font-mono font-semibold text-white tabular-nums">{phase.ms}ms</span>
        </div>
      {/each}
      {#if computedTotal > 0}
        <div class="flex items-center gap-1.5 text-xs">
          <span class="text-[var(--fp-dim)]">total</span>
          <span class="font-mono font-semibold text-white tabular-nums">{computedTotal}ms</span>
        </div>
      {/if}
    </div>
  </div>
{/if}
