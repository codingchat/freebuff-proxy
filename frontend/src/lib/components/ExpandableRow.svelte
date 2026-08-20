<script>
  /**
   * ExpandableRow — table row that expands to show detail content.
   *
   * @prop {boolean} [expanded=false]
   * @prop {function} [ontoggle]
   */
  let { expanded = false, ontoggle, children, detail } = $props();
</script>

{@render children({ expanded, ontoggle })}
{#if expanded}
  <tr class="bg-[var(--fp-surface-2)]">
    <td colspan="999" class="!p-0">
      <div class="px-4 py-3 border-t border-[var(--fp-border)]">
        {@render detail()}
      </div>
    </td>
  </tr>
{/if}
