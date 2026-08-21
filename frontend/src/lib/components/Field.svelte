<script>
  /**
   * Field — labeled form control wrapper.
   *
   * @prop {string} label
   * @prop {string} [hint]
   * @prop {string} [error]
   * @prop {string} id — wired to the control via `for` and to hint/error via {id}-hint/{id}-error
   */
  let { label, hint, error, id, children } = $props();
</script>

<div class="flex flex-col gap-1.5">
  {#if label}
    <label for={id} class="text-xs text-[var(--fp-muted)]">{label}</label>
  {/if}
  {#if children}
    {@render children()}
  {/if}
  {#if error}
    <p class="text-[11px] text-[var(--fp-error)]" id="{id}-error">{error}</p>
  {:else if hint}
    <p class="text-[11px] text-[var(--fp-dim)]" id="{id}-hint">{hint}</p>
  {/if}
</div>
