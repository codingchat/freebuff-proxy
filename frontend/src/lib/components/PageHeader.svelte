<script>
  /**
   * PageHeader — page title + description + optional right-side actions.
   *
   * @prop {string} title
   * @prop {string} [description]
   * @slot actions
   */
  let { title, description, actions } = $props();
</script>

<header class="flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between">
  <div>
    <h1 class="text-[20px] font-semibold text-[var(--fp-text)]">{title}</h1>
    {#if description}
      <p class="mt-1 text-sm text-[var(--fp-muted)]">{description}</p>
    {/if}
  </div>
  {#if actions}
    <div class="flex flex-wrap items-center gap-2">
      {@render actions()}
    </div>
  {/if}
</header>
