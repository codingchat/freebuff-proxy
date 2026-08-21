<script>
  import { onMount } from 'svelte';
  import Sidebar from './lib/Sidebar.svelte';
  import Footer from './lib/Footer.svelte';
  import Overview from './lib/pages/Overview.svelte';
  import Tokens from './lib/pages/Tokens.svelte';
  import Models from './lib/pages/Models.svelte';
  import Config from './lib/pages/Config.svelte';
  import Logs from './lib/pages/Logs.svelte';
  import Setup from './lib/pages/Setup.svelte';
  import Login from './lib/pages/Login.svelte';

  function getInitialTab() {
    if (typeof window === 'undefined') return 'overview';
    const path = window.location.pathname;
    const hash = window.location.hash.replace('#', '');
    if (path === '/admin/login' || hash === 'login') return 'login';
    if (hash) return hash;
    const segments = path.split('/').filter(Boolean);
    if (segments.length >= 2 && segments[0] === 'admin' && segments[1]) {
      return segments[1];
    }
    return 'overview';
  }

  let activeTab = $state(getInitialTab());
  let versionInfo = $state(null);

  function syncTabFromURL() {
    activeTab = getInitialTab();
  }

  $effect(() => {
    if (activeTab !== 'login' && window.location.hash.replace('#', '') !== activeTab) {
      window.location.hash = activeTab;
    }
  });

  onMount(() => {
    syncTabFromURL();
    window.addEventListener('hashchange', syncTabFromURL);

    // Fetch version / update check
    fetch('/admin/api/version')
      .then((res) => res.json())
      .then((data) => {
        versionInfo = {
          current_version: data.current_version || '',
          has_update: data.has_update || false,
          latest_version: data.latest_version || '',
          update_url: data.update_url || '',
        };
      })
      .catch((e) => console.warn('version check failed', e));

    return () => {
      window.removeEventListener('hashchange', syncTabFromURL);
    };
  });
</script>

<div class="min-h-screen bg-[var(--fp-bg)] text-[var(--fp-text)] flex flex-col font-sans selection:bg-[var(--fp-accent)]/30 selection:text-white instrument-grid">
  <a
    href="#main-content"
    class="sr-only focus:not-sr-only focus:fixed focus:top-3 focus:left-3 focus:z-[60] focus:px-4 focus:py-2 focus:rounded-lg focus:bg-[var(--fp-accent)] focus:text-[var(--fp-bg)] focus:font-semibold focus:text-sm"
  >
    Skip to content
  </a>

  {#if activeTab !== 'login'}
    <Sidebar bind:activeTab {versionInfo} />
  {/if}

  <div class="flex-1 flex flex-col {activeTab !== 'login' ? 'md:pl-56' : ''}">
    <main id="main-content" class="flex-1 w-full max-w-[1200px] mx-auto px-6 py-8">
      {#key activeTab}
        <div class="page-enter">
          {#if activeTab === 'overview'}
            <Overview />
          {:else if activeTab === 'tokens'}
            <Tokens />
          {:else if activeTab === 'models'}
            <Models />
          {:else if activeTab === 'config'}
            <Config />
          {:else if activeTab === 'logs'}
            <Logs />
          {:else if activeTab === 'setup'}
            <Setup />
          {:else if activeTab === 'login'}
            <Login />
          {/if}
        </div>
      {/key}
    </main>

    {#if activeTab !== 'login'}
      <Footer {versionInfo} />
    {/if}
  </div>
</div>
