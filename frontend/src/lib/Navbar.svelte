<script>
  import {
    LayoutDashboard, Key, Cpu, Activity, MessageSquare,
    Settings, Wrench, FileText, BarChart3, ChevronRight,
    Menu, X, ArrowUpCircle, LogOut
  } from '@lucide/svelte';

  /**
   * @prop {string} activeTab
   * @prop {(tab: string) => void} onTabChange
   * @prop {{ has_update: boolean, latest_version: string, update_url: string }} [versionInfo]
   */
  let { activeTab = $bindable(), onTabChange, versionInfo } = $props();

  let mobileOpen = $state(false);
  let desktopTabEls = $state([]);
  let mobileTabEls = $state([]);

  const tabs = [
    { id: 'overview',   label: 'Overview',    icon: LayoutDashboard },
    { id: 'tokens',     label: 'Tokens',      icon: Key },
    { id: 'models',     label: 'Models',      icon: Cpu },
    { id: 'traces',     label: 'Traces',      icon: Activity },
    { id: 'playground', label: 'Playground',  icon: MessageSquare },
    { id: 'config',     label: 'Config',      icon: Settings },
    { id: 'setup',      label: 'Setup',       icon: Wrench },
    { id: 'logs',       label: 'Logs',        icon: FileText },
    { id: 'metrics',    label: 'Metrics',     icon: BarChart3 },
  ];

  function switchTab(id) {
    activeTab = id;
    onTabChange?.(id);
    window.location.hash = id;
    mobileOpen = false;
  }

  // ARIA tabs pattern: Arrow keys move selection and focus (roving tabindex).
  // `els` is the ref array of the originating list (desktop or mobile drawer),
  // so focus always lands on a mounted, visible button of that list.
  function handleTabKeydown(e, i, els) {
    const last = tabs.length - 1;
    let next = null;
    if (e.key === 'ArrowRight') next = i === last ? 0 : i + 1;
    else if (e.key === 'ArrowLeft') next = i === 0 ? last : i - 1;
    else if (e.key === 'Home') next = 0;
    else if (e.key === 'End') next = last;
    if (next === null) return;
    e.preventDefault();
    switchTab(tabs[next].id);
    els[next]?.focus();
  }

  function handleDrawerKeydown(e) {
    if (e.key === 'Escape') mobileOpen = false;
  }

  // Log out: clear the fb_admin cookie server-side, then land on the login page.
  // Planned navigation wraps the fetch so logout still works if the endpoint is
  // unreachable or ADMIN_TOKEN is unset (server GET /admin/logout redirects).
  async function logout() {
    try {
      await fetch('/admin/logout', { method: 'POST' });
    } catch {
      // ignore — navigate regardless
    }
    window.location.href = '/admin/login';
  }
</script>

<header
  class="sticky top-0 z-50 border-b border-[var(--fp-border)] bg-[var(--fp-bg)]/80 backdrop-blur-xl backdrop-saturate-150"
>
  <nav class="max-w-7xl mx-auto px-4 sm:px-6" aria-label="Main navigation">
    <div class="flex items-center justify-between h-14">
      <!-- Logo -->
      <div class="flex items-center gap-3">
        <a href="/admin" class="flex items-center gap-2.5 group" aria-label="freebuff-proxy dashboard home">
          <div class="w-7 h-7 rounded-lg bg-[var(--fp-amber)]/12 border border-[var(--fp-amber)]/25 flex items-center justify-center transition-all duration-200 group-hover:bg-[var(--fp-amber)]/20 group-hover:border-[var(--fp-amber)]/40">
            <ChevronRight size={14} class="text-[var(--fp-amber)]" />
          </div>
          <span class="text-sm font-semibold text-white tracking-tight hidden sm:inline">freebuff-proxy</span>
        </a>
      </div>

      <!-- Desktop Nav -->
      <div class="hidden md:flex items-center gap-0.5" role="tablist" aria-label="Dashboard sections">
        {#each tabs as tab, i}
          <button
            bind:this={desktopTabEls[i]}
            role="tab"
            aria-selected={activeTab === tab.id}
            tabindex={activeTab === tab.id ? 0 : -1}
            onclick={() => switchTab(tab.id)}
            onkeydown={(e) => handleTabKeydown(e, i, desktopTabEls)}
            class="relative flex items-center gap-1.5 px-3 py-2 rounded-lg text-xs font-medium transition-all duration-200
              {activeTab === tab.id
                ? 'text-[var(--fp-amber)] bg-[var(--fp-amber)]/10'
                : 'text-[var(--fp-muted)] hover:text-white hover:bg-[var(--fp-surface-3)]'}"
          >
            <tab.icon size={14} />
            <span>{tab.label}</span>
            {#if activeTab === tab.id}
              <span class="absolute -bottom-[9px] left-3 right-3 h-[2px] bg-[var(--fp-amber)] rounded-full"></span>
            {/if}
          </button>
        {/each}
      </div>

      <!-- Right side: update badge + mobile menu -->
      <div class="flex items-center gap-2">
        {#if versionInfo?.has_update}
          <a
            href={versionInfo.update_url}
            target="_blank"
            rel="noopener noreferrer"
            class="hidden sm:inline-flex items-center gap-1.5 px-2.5 py-1 rounded-full text-[10px] font-semibold uppercase tracking-wider bg-[var(--fp-teal)]/15 text-[var(--fp-teal)] border border-[var(--fp-teal)]/30 hover:bg-[var(--fp-teal)]/25 transition-colors"
          >
            <ArrowUpCircle size={12} />
            <span>v{versionInfo.latest_version}</span>
          </a>
        {/if}

        <!-- Log out (desktop) -->
        <button
          onclick={logout}
          class="hidden md:inline-flex items-center gap-1.5 px-2.5 py-1.5 rounded-lg text-xs font-medium text-[var(--fp-muted)] hover:text-white hover:bg-[var(--fp-surface-3)] transition-colors"
          aria-label="Log out"
        >
          <LogOut size={14} />
          <span>Log out</span>
        </button>

        <!-- Mobile menu button -->
        <button
          class="md:hidden p-2.5 min-w-11 min-h-11 rounded-lg text-[var(--fp-muted)] hover:text-white hover:bg-[var(--fp-surface-3)] transition-colors flex items-center justify-center"
          onclick={() => mobileOpen = !mobileOpen}
          aria-label={mobileOpen ? 'Close menu' : 'Open menu'}
          aria-expanded={mobileOpen}
          aria-controls="mobile-nav"
        >
          {#if mobileOpen}
            <X size={20} />
          {:else}
            <Menu size={20} />
          {/if}
        </button>
      </div>
    </div>

    <!-- Mobile Nav -->
    {#if mobileOpen}
      <div
        id="mobile-nav"
        class="md:hidden py-3 border-t border-[var(--fp-border)] space-y-1"
        tabindex="-1"
        role="tablist"
        aria-label="Mobile navigation"
        onkeydown={handleDrawerKeydown}
      >
        {#each tabs as tab, i}
          <button
            bind:this={mobileTabEls[i]}
            role="tab"
            aria-selected={activeTab === tab.id}
            tabindex={activeTab === tab.id ? 0 : -1}
            onclick={() => switchTab(tab.id)}
            onkeydown={(e) => handleTabKeydown(e, i, mobileTabEls)}
            class="w-full min-h-11 flex items-center gap-3 px-3 py-2.5 rounded-lg text-sm font-medium transition-colors
              {activeTab === tab.id
                ? 'text-[var(--fp-amber)] bg-[var(--fp-amber)]/10'
                : 'text-[var(--fp-muted)] hover:text-white hover:bg-[var(--fp-surface-3)]'}"
          >
            <tab.icon size={16} />
            <span>{tab.label}</span>
          </button>
        {/each}

        {#if versionInfo?.has_update}
          <a
            href={versionInfo.update_url}
            target="_blank"
            rel="noopener noreferrer"
            class="flex min-h-11 items-center gap-2 px-3 py-2.5 rounded-lg text-sm font-medium text-[var(--fp-teal)] hover:bg-[var(--fp-teal)]/10 transition-colors"
          >
            <ArrowUpCircle size={16} />
            <span>Update to v{versionInfo.latest_version}</span>
          </a>
        {/if}

        <!-- Log out (mobile drawer) -->
        <button
          onclick={logout}
          class="w-full min-h-11 flex items-center gap-3 px-3 py-2.5 rounded-lg text-sm font-medium text-[var(--fp-muted)] hover:text-white hover:bg-[var(--fp-surface-3)] transition-colors"
        >
          <LogOut size={16} />
          <span>Log out</span>
        </button>
      </div>
    {/if}
  </nav>
</header>
