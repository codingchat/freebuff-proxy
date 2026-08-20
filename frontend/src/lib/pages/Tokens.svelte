<script>
  import { onDestroy } from 'svelte';
  import { Key, Unlock, Zap, Plus, Trash2, Layers, Network, Server, Sparkles, RefreshCw, ExternalLink, ChevronDown, ChevronRight } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import Alert from '../components/Alert.svelte';
  import ThresholdBar from '../components/ThresholdBar.svelte';
  import { fetchAPI, postAPI } from '../utils/api.js';
  import { usePolling } from '../utils/polling.js';
  import { formatLocalDate, generateRandomApiKey } from '../utils/format.js';

  let data = $state(null);
  let loading = $state(true);
  let error = $state('');

  let newToken = $state('');
  let actionMessage = $state('');
  let actionOK = $state(true);
  let actionPending = $state(false);
  let clientKeyMessage = $state('');
  let clientKeyOK = $state(true);
  let generatingKey = $state(false);
  let apiKeys = $state([]);
  let copiedKeyIdx = $state(null);

  // Expandable token rows
  let expandedToken = $state(null);

  // Token validation state (as user types)
  let tokenValid = $derived(
    newToken.trim() === ''
      ? null
      : /^cb_[A-Za-z0-9_-]{20,}$/.test(newToken.trim())
  );

  // Test-all results: { [tokenIndex]: { ok, latencyMs, model, instance, ts } }
  let testResults = $state({});

  // OAuth wizard state
  let oauthStarting = $state(false);
  let oauthStatus = $state(null);
  let oauthTimer = $state(null);

  async function startOAuthLogin() {
    oauthStarting = true;
    oauthStatus = { message: 'Starting headless login flow...', type: 'info' };

    try {
      const res = await fetch('/admin/login/start', { method: 'POST' });
      const result = await res.json();

      if (result.fingerprint && result.login_url) {
        oauthStatus = {
          loginUrl: result.login_url,
          fingerprint: result.fingerprint,
          message: 'Open this URL in your browser to sign in:',
          type: 'pending'
        };

        clearInterval(oauthTimer);
        oauthTimer = setInterval(async () => {
          try {
            const pollRes = await fetch(`/admin/login/status?fingerprint=${encodeURIComponent(result.fingerprint)}`);
            const pollData = await pollRes.json();

            if (pollData.status === 'completed') {
              clearInterval(oauthTimer);
              oauthStatus = {
                message: `✓ Token #${pollData.token_index} added to pool and saved to .env.`,
                type: 'success'
              };
              oauthStarting = false;
              fetchData();
            } else if (pollData.status === 'error') {
              clearInterval(oauthTimer);
              oauthStatus = {
                message: `Login failed: ${pollData.message || 'unknown error'}`,
                type: 'error'
              };
              oauthStarting = false;
            }
          } catch {
            // keep polling
          }
        }, 3000);
      } else {
        oauthStatus = {
          message: result.message || 'Failed to start login wizard.',
          type: 'error'
        };
        oauthStarting = false;
      }
    } catch (e) {
      oauthStatus = { message: `Network error: ${e.message}`, type: 'error' };
      oauthStarting = false;
    }
  }

  async function fetchData() {
    try {
      data = await fetchAPI('/admin/api/tokens');
      try {
        const cfgRes = await fetchAPI('/admin/api/config');
        const envContent = cfgRes?.env_content || '';
        const m = envContent.match(/^\s*API_KEYS=(.*)$/m);
        const val = m ? m[1].trim() : '';
        apiKeys = val ? val.split(',').map(s => s.trim()).filter(Boolean) : [];
      } catch {
        apiKeys = [];
      }
      error = '';
    } catch (e) {
      error = e.message || 'Failed to fetch tokens';
    } finally {
      loading = false;
    }
  }

  async function generateClientKey() {
    if (generatingKey) return;
    generatingKey = true;
    clientKeyMessage = '';
    try {
      const newKey = generateRandomApiKey();
      const cfgRes = await fetchAPI('/admin/api/config');
      const envContent = cfgRes?.env_content || '';
      const regex = /^\s*API_KEYS=(.*)$/m;
      const match = envContent.match(regex);
      const existing = match ? match[1].trim() : '';
      const updated = existing ? `${existing},${newKey}` : newKey;
      const save = await fetch('/admin/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ content: envContent.replace(regex, `API_KEYS=${updated}`) }),
      });
      const result = await save.json();
      clientKeyOK = save.ok && result.ok;
      clientKeyMessage = clientKeyOK
        ? `Generated & saved client API key: ${newKey}`
        : (result.message || 'Failed to save client API key');
      if (clientKeyOK) {
        try { await navigator.clipboard.writeText(newKey); } catch { /* clipboard unavailable */ }
        fetchData();
      }
    } catch (e) {
      clientKeyOK = false;
      clientKeyMessage = e.message || 'Network error generating client key';
    } finally {
      generatingKey = false;
    }
  }

  async function copyKey(idx) {
    const key = apiKeys[idx];
    if (!key) return;
    try {
      await navigator.clipboard.writeText(key);
      copiedKeyIdx = idx;
      setTimeout(() => { if (copiedKeyIdx === idx) copiedKeyIdx = null; }, 1800);
    } catch { /* clipboard unavailable */ }
  }

  async function addToken(e) {
    e.preventDefault();
    if (!newToken.trim() || actionPending) return;
    actionPending = true;
    try {
      const result = await postAPI('/admin/tokens/add', { token: newToken.trim() });
      actionOK = result.ok !== false;
      actionMessage = result.message || (actionOK ? 'Token added successfully' : 'Failed to add token');
      if (actionOK) { newToken = ''; fetchData(); }
    } catch (e) {
      actionOK = false;
      actionMessage = e.message || 'Network error adding token';
    } finally {
      actionPending = false;
    }
  }

  async function triggerAction(url, body, confirmMsg) {
    if (confirmMsg && !confirm(confirmMsg)) return;
    actionPending = true;
    try {
      const result = await postAPI(url, body || undefined);
      actionOK = result.ok !== false;
      actionMessage = result.message || (actionOK ? 'Action completed' : 'Action failed');
      fetchData();
    } catch (e) {
      actionOK = false;
      actionMessage = e.message || 'Network error executing action';
    } finally {
      actionPending = false;
    }
  }

  // Test a single token and record result
  async function testToken(idx) {
    const start = performance.now();
    try {
      const result = await postAPI(`/admin/tokens/${idx}/test`, {});
      const latencyMs = Math.round(performance.now() - start);
      testResults = {
        ...testResults,
        [idx]: {
          ok: result.ok !== false,
          latencyMs,
          model: result.model || result.model_id || '—',
          instance: result.instance || result.instance_id || '—',
          ts: Date.now()
        }
      };
      actionOK = result.ok !== false;
      actionMessage = result.message || (result.ok !== false ? `Token #${idx} test passed` : `Token #${idx} test failed`);
    } catch (e) {
      const latencyMs = Math.round(performance.now() - start);
      testResults = {
        ...testResults,
        [idx]: { ok: false, latencyMs, model: '—', instance: '—', ts: Date.now() }
      };
      actionOK = false;
      actionMessage = e.message || 'Network error testing token';
    }
  }

  // Test all tokens and collect results
  async function testAllTokens() {
    if (!data?.tokens?.length) return;
    actionPending = true;
    testResults = {};
    const tokens = data.tokens;
    for (let i = 0; i < tokens.length; i++) {
      const idx = tokens[i].index ?? i;
      const start = performance.now();
      try {
        const result = await postAPI(`/admin/tokens/${idx}/test`, {});
        const latencyMs = Math.round(performance.now() - start);
        testResults = {
          ...testResults,
          [idx]: {
            ok: result.ok !== false,
            latencyMs,
            model: result.model || result.model_id || '—',
            instance: result.instance || result.instance_id || '—',
            ts: Date.now()
          }
        };
      } catch {
        const latencyMs = Math.round(performance.now() - start);
        testResults = {
          ...testResults,
          [idx]: { ok: false, latencyMs, model: '—', instance: '—', ts: Date.now() }
        };
      }
    }
    actionPending = false;
    actionOK = true;
    actionMessage = 'Test complete for all tokens';
  }

  function toggleExpand(idx) {
    expandedToken = expandedToken === idx ? null : idx;
  }

  function handleModeSwitch(targetMode) {
    if (!data) return;
    const current = data.in_bridge ? 'bridge' : 'pooled';
    if (current === targetMode) return;

    if (targetMode === 'pooled') {
      if (!data.has_tokens) {
        actionOK = false;
        actionMessage = 'Pooled mode requires at least one token. Paste a token into the form below first.';
        return;
      }
      triggerAction('/admin/mode', { mode: 'pooled' }, 'Switch to pooled mode? All client requests will share the server pool.');
    } else if (targetMode === 'bridge') {
      triggerAction('/admin/mode', { mode: 'bridge' }, 'Switch to bridge mode? Pooled tokens are cleared from memory and .env; clients send their own credentials.');
    }
  }

  usePolling(fetchData, 30000);

  onDestroy(() => {
    clearInterval(oauthTimer);
  });

  function modeVariant(d) {
    if (d?.in_bridge) return 'blue';
    return 'amber';
  }

  function riskVariant(risk) {
    if (risk === 'low') return 'teal';
    if (risk === 'moderate') return 'amber';
    return 'red';
  }

  function riskDot(risk) {
    if (risk === 'low') return 'bg-[var(--fp-teal)]';
    if (risk === 'moderate') return 'bg-[var(--fp-amber)]';
    return 'bg-[var(--fp-red)]';
  }
</script>

<div class="space-y-6 page-enter">
  <PageHeader title="Tokens & Quotas" subtitle="Manage upstream credentials, runtime routing modes, and per-model quotas">
    {#if data}
      <StatusBadge variant={modeVariant(data)}>{data.mode} mode</StatusBadge>
      {#if data.in_bridge}
        <StatusBadge variant="muted" mono>{data.bridge_tokens} client{data.bridge_tokens === 1 ? '' : 's'}</StatusBadge>
      {/if}
    {/if}
  </PageHeader>

  <!-- Action Status -->
  {#if actionMessage}
    <Alert
      variant={actionOK ? 'success' : 'error'}
      message={actionMessage}
      ondismiss={() => actionMessage = ''}
    />
  {/if}

  <!-- Fetch Error -->
  {#if error}
    <Alert
      variant="error"
      message={error}
      ondismiss={() => error = ''}
    />
  {/if}

  <!-- Mode Control & Pool Routing Bar -->
  <div class="fp-card p-5 space-y-4">
    <div class="flex flex-col lg:flex-row lg:items-center justify-between gap-4">
      <div>
        <div class="flex items-center gap-2">
          <Layers size={18} class="text-[var(--fp-amber)]" />
          <h2 class="text-base font-semibold text-white">
            Routing Mode & Pool Configuration
            {#if data}
              <span class="text-xs font-normal text-[var(--fp-muted)] ml-1.5">
                ({data.token_count || 0} pooled token{data.token_count === 1 ? '' : 's'})
              </span>
            {/if}
          </h2>
        </div>
        <p class="text-xs text-[var(--fp-muted)] mt-1">
          Select how the proxy routes incoming requests to upstream models. Changes apply immediately and save to <code class="px-1.5 py-0.5 rounded fp-inset text-[var(--fp-amber)] font-mono">.env</code>.
        </p>
      </div>

      <div class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          onclick={() => handleModeSwitch('pooled')}
          disabled={!data}
          class="px-3.5 py-2 rounded-lg text-xs font-semibold transition-all flex items-center gap-1.5 disabled:cursor-not-allowed disabled:opacity-40
            {data && !data.in_bridge
              ? 'bg-[var(--fp-amber)]/20 text-[var(--fp-amber)] border border-[var(--fp-amber)]/50 shadow-sm'
              : 'bg-[var(--fp-surface-3)] hover:enabled:bg-[var(--fp-border-bright)] text-[var(--fp-muted)] hover:enabled:text-white border border-[var(--fp-border)]'}"
          title="All requests share the server token pool"
        >
          <Server size={14} />
          <span>Pooled</span>
          {data && !data.in_bridge ? '✓' : ''}
        </button>

        <button
          type="button"
          onclick={() => handleModeSwitch('bridge')}
          disabled={!data}
          class="px-3.5 py-2 rounded-lg text-xs font-semibold transition-all flex items-center gap-1.5 disabled:cursor-not-allowed disabled:opacity-40
            {data?.in_bridge
              ? 'bg-[#60A5FA]/20 text-[#60A5FA] border border-[#60A5FA]/50 shadow-sm'
              : 'bg-[var(--fp-surface-3)] hover:enabled:bg-[var(--fp-border-bright)] text-[var(--fp-muted)] hover:enabled:text-white border border-[var(--fp-border)]'}"
          title="Stateless mode: clients provide their own tokens"
        >
          <Network size={14} />
          <span>Bridge</span>
          {data?.in_bridge ? '✓' : ''}
        </button>

        {#if data?.has_tokens}
          <div class="h-6 w-[1px] bg-[var(--fp-border)] mx-1 hidden sm:block"></div>
          <button
            type="button"
            onclick={testAllTokens}
            disabled={actionPending}
            class="fp-btn-secondary text-[var(--fp-amber)] border-[var(--fp-amber)]/30"
          >
            {#if actionPending}
              <RefreshCw size={13} class="animate-spin" />
            {:else}
              <Zap size={13} />
            {/if}
            <span>Test All</span>
          </button>
          <button
            type="button"
            onclick={() => triggerAction('/admin/tokens/remove', {}, 'Remove the last token from the pool and .env?')}
            class="fp-btn-danger"
          >
            <Trash2 size={13} />
            <span>Remove Last</span>
          </button>
        {/if}
      </div>
    </div>

    <!-- Active Mode Description Banner -->
    <div class="p-3 rounded-lg fp-inset text-xs flex items-center justify-between gap-2">
      {#if data?.in_bridge}
        <span class="text-[var(--fp-muted)]">
          <strong class="text-[#60A5FA]">Bridge Mode Active:</strong> Server pool is empty. Clients must provide their own FreeBuff token per request (<code class="font-mono text-[var(--fp-text)]">Authorization: Bearer cb_...</code>).
        </span>
      {:else if data}
        <span class="text-[var(--fp-muted)]">
          <strong class="text-[var(--fp-amber)]">Pooled Mode Active:</strong> All client requests share the {data.token_count || 0} server token(s) with automatic load rotation and anti-ban safeguards.
        </span>
      {:else}
        <span class="text-[var(--fp-muted)]">
          {loading ? 'Loading pool data...' : 'Pool data unavailable right now.'}
        </span>
      {/if}
    </div>
  </div>

  <!-- Headless OAuth Token Generator -->
  <div class="fp-card p-5 space-y-3 border-[var(--fp-amber)]/30">
    <div class="flex items-center justify-between">
      <div class="flex items-center gap-2">
        <Sparkles size={18} class="text-[var(--fp-amber)]" />
        <h2 class="text-base font-semibold text-white">Headless OAuth Token Generator</h2>
      </div>
    </div>
    <p class="text-xs text-[var(--fp-muted)]">
      Generate FreeBuff credentials directly in your browser without installing the Codebuff CLI. The token is validated against upstream, then added to the pool and saved to <code class="text-[var(--fp-amber)] font-mono">.env</code>.
    </p>

    <button
      onclick={startOAuthLogin}
      disabled={oauthStarting}
      class="fp-btn-primary"
    >
      {#if oauthStarting}
        <RefreshCw size={14} class="animate-spin" />
        <span>Authorizing...</span>
      {:else}
        <Sparkles size={14} />
        <span>Generate Token via Browser Login</span>
      {/if}
    </button>

    {#if oauthStatus}
      <div class="mt-3 p-4 rounded-lg fp-inset text-xs font-mono space-y-2">
        <p class="text-white">{oauthStatus.message}</p>
        {#if oauthStatus.loginUrl}
          <div class="flex items-center gap-2">
            <a
              href={oauthStatus.loginUrl}
              target="_blank"
              rel="noopener noreferrer"
              class="px-3 py-1.5 rounded bg-[var(--fp-amber)]/10 border border-[var(--fp-amber)]/30 text-[var(--fp-amber)] hover:underline flex items-center gap-1.5"
            >
              <span>{oauthStatus.loginUrl}</span>
              <ExternalLink size={12} />
            </a>
          </div>
        {/if}
      </div>
    {/if}
  </div>

  <!-- Client API Key Card -->
  <div class="fp-card p-5 border-[var(--fp-teal)]/30">
    <h2 class="text-base font-semibold text-white mb-1">Client API Key</h2>
    <p class="text-xs text-[var(--fp-muted)] mb-3">Generate a <code class="font-mono text-[var(--fp-teal)]">sk-fb-...</code> credential for clients (<code class="font-mono">omp</code>, curl) to authenticate against this proxy. Appended to <code class="font-mono text-[var(--fp-teal)]">API_KEYS</code> in <code class="font-mono">.env</code>.</p>

    {#if apiKeys.length > 0}
      <div class="space-y-2 mb-3">
        <p class="text-[10px] uppercase tracking-wider text-[var(--fp-dim)] font-semibold">Existing keys, click to copy for another client</p>
        {#each apiKeys as key, idx}
          <button
            type="button"
            onclick={() => copyKey(idx)}
            title="Click to copy"
            class="w-full flex items-center justify-between gap-2 p-2.5 rounded-lg fp-inset text-left font-mono text-xs hover:border-[var(--fp-teal)]/40 transition-all focus-visible:ring-2 focus-visible:ring-[var(--fp-teal)]"
          >
            <span class="truncate text-[var(--fp-teal)]">{key}</span>
            <span class="shrink-0 text-[var(--fp-muted)]">
              {copiedKeyIdx === idx ? '✓ copied' : 'copy'}
            </span>
          </button>
        {/each}
      </div>
    {/if}

    {#if clientKeyMessage}
      <Alert variant={clientKeyOK ? 'success' : 'error'} message={clientKeyMessage} dismissable={false} />
    {/if}
    <button
      onclick={generateClientKey}
      disabled={generatingKey}
      class="fp-btn-primary bg-[var(--fp-teal)] border-[var(--fp-teal)] text-[#0A0F18]"
    >
      <Key size={16} />
      <span>{generatingKey ? 'Generating...' : 'Generate Client API Key'}</span>
    </button>
  </div>

  <!-- Add Token Card (A→B→C→D) -->
  <div class="fp-card p-5 border-[var(--fp-amber)]/30">
    <!-- A: Validation status indicator -->
    <h2 class="text-base font-semibold text-white mb-1 flex items-center gap-2">
      Add Token to Pool
      {#if tokenValid === true}
        <span class="text-[var(--fp-teal)] text-xs font-normal flex items-center gap-1">
          <span class="w-4 h-4 rounded-full bg-[var(--fp-teal)]/20 flex items-center justify-center text-[10px]">✓</span>
          valid format
        </span>
      {:else if tokenValid === false}
        <span class="text-[var(--fp-red)] text-xs font-normal flex items-center gap-1">
          <span class="w-4 h-4 rounded-full bg-[var(--fp-red)]/20 flex items-center justify-center text-[10px]">✗</span>
          invalid format
        </span>
      {/if}
    </h2>
    <!-- D: help text -->
    <p class="text-xs text-[var(--fp-muted)] mb-3">Token must be a valid FreeBuff auth token (<code class="font-mono text-[var(--fp-amber)]">cb_...</code>). Adding burns no quota.</p>
    <!-- C: Add button + input -->
    <form onsubmit={addToken} class="flex flex-col sm:flex-row gap-2">
      <input
        type="text"
        bind:value={newToken}
        placeholder="Paste FreeBuff token (cb_...)"
        autocomplete="off"
        spellcheck="false"
        class="fp-input fp-input-mono flex-1
          {tokenValid === false ? 'border-[var(--fp-red)]/60 focus:border-[var(--fp-red)]' : ''}
          {tokenValid === true ? 'border-[var(--fp-teal)]/60 focus:border-[var(--fp-teal)]' : ''}"
      />
      <button
        type="submit"
        disabled={actionPending || !newToken.trim() || tokenValid === false}
        class="fp-btn-primary"
      >
        <Plus size={16} />
        <span>Add Token</span>
      </button>
    </form>
  </div>

  <!-- Token Details List (Expandable Rows) -->
  <div class="space-y-2">
    {#each data?.tokens || [] as token, i (token.index ?? i)}
      {@const isExpanded = expandedToken === (token.index ?? i)}
      {@const tr = testResults[token.index ?? i]}

      <!-- Token Row (A→B→C→D) -->
      <div class="fp-card overflow-hidden transition-all duration-200 {isExpanded ? 'ring-1 ring-[var(--fp-amber)]/30' : ''}">
        <!-- Collapsed header — clickable div (not button) to allow nested buttons -->
        <div
          role="button"
          tabindex="0"
          onclick={() => toggleExpand(token.index ?? i)}
          onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleExpand(token.index ?? i); } }}
          class="w-full p-4 flex items-center gap-3 text-left hover:bg-[var(--fp-surface)]/50 transition-colors cursor-pointer select-none"
        >
          <!-- D: Expand indicator -->
          <span class="shrink-0 text-[var(--fp-dim)]">
            {#if isExpanded}
              <ChevronDown size={16} />
            {:else}
              <ChevronRight size={16} />
            {/if}
          </span>

          <!-- A: Risk dot + badge -->
          <div class="w-3 h-3 rounded-full shrink-0 {riskDot(token.risk_level)}"></div>
          <span class="text-sm font-bold text-white font-mono shrink-0">Token #{token.index}</span>
          <StatusBadge variant={riskVariant(token.risk_level)}>{token.risk_level}</StatusBadge>
          {#if token.has_standing}
            <StatusBadge variant="blue" uppercase={false} class="hidden sm:inline-flex">
              trust {token.standing_label} ({Math.round(token.standing_score)}/100)
            </StatusBadge>
          {/if}

          <!-- B: Stats inline (desktop) -->
          <div class="hidden md:flex items-center gap-4 ml-auto text-[11px] font-mono text-[var(--fp-muted)] shrink-0">
            <span>runs: <span class="text-white tabular-nums">{token.active_runs}</span></span>
            <span>req: <span class="text-white tabular-nums">{token.requests}</span></span>
            <span>24h: <span class="text-white tabular-nums">{token.messages_24h}</span></span>
          </div>

          <!-- C: Action buttons -->
          <div class="flex items-center gap-1.5 shrink-0 ml-2" onclick={(e) => e.stopPropagation()}>
            {#if token.cooldown_active}
              <button
                onclick={() => triggerAction(`/admin/tokens/${token.index}/unlock`, {}, `Unlock Token ${token.index}? Only do this if the lock is stale.`)}
                class="fp-btn-secondary text-[var(--fp-teal)] border-[var(--fp-teal)]/30 !py-1 !px-2 !text-[11px]"
                title="Unlock this token"
              >
                <Unlock size={11} />
              </button>
            {/if}
            <button
              onclick={() => testToken(token.index ?? i)}
              class="fp-btn-secondary !py-1 !px-2 !text-[11px]"
              title="Test this token"
            >
              <Zap size={11} />
            </button>
            <button
              onclick={() => triggerAction(`/admin/tokens/${token.index}/finish`, {}, `Finish all active runs for Token ${token.index}?`)}
              class="fp-btn-secondary text-[var(--fp-muted)] !py-1 !px-2 !text-[11px]"
              title="Finish active runs"
            >
              Finish
            </button>
          </div>
        </div>

        <!-- Mobile stats row (shown below header on small screens) -->
        <div class="md:hidden px-4 pb-3 flex items-center gap-3 text-[11px] font-mono text-[var(--fp-muted)]">
          <span>runs: <span class="text-white tabular-nums">{token.active_runs}</span></span>
          <span>req: <span class="text-white tabular-nums">{token.requests}</span></span>
          <span>24h: <span class="text-white tabular-nums">{token.messages_24h}{token.daily_limit > 0 ? `/${token.daily_limit}` : ''}</span></span>
        </div>

        <!-- Expanded Detail -->
        {#if isExpanded}
          <div class="border-t border-[var(--fp-border)] bg-[var(--fp-surface)]/30 px-4 py-4 space-y-4">
            <!-- A: Access tier badge -->
            <div class="flex items-center gap-3">
              <span class="text-xs text-[var(--fp-dim)] uppercase tracking-wider font-semibold">Access Tier</span>
              <StatusBadge variant={token.access_tier === 'full' ? 'teal' : 'amber'}>
                {token.access_tier || 'unknown'}
              </StatusBadge>
              {#if token.session_instance}
                <span class="text-[11px] font-mono text-[var(--fp-muted)] ml-auto">
                  Instance: <span class="text-white">{token.session_instance}</span>
                </span>
              {/if}
            </div>

            <!-- B: Per-model quota breakdown -->
            {#if token.has_quota && token.quota?.length > 0}
              <div class="space-y-3">
                <h4 class="text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider">Model Quotas</h4>
                <div class="grid gap-3">
                  {#each token.quota as q}
                    <div class="p-3 rounded-lg fp-inset space-y-2">
                      <div class="flex items-center justify-between text-xs">
                        <span class="font-bold text-white font-mono">{q.model}</span>
                        <span class="text-[var(--fp-muted)]">{q.period}</span>
                      </div>
                      <div class="flex items-center justify-between text-xs font-mono">
                        <span class="text-[var(--fp-muted)]">
                          Used: <span class="text-white tabular-nums">{q.recent}</span> / <span class="text-white tabular-nums">{q.limit}</span>
                        </span>
                        <span class="text-[var(--fp-muted)]">
                          Remaining: <span class="text-white tabular-nums">{Math.max(0, q.limit - q.recent)}</span>
                        </span>
                      </div>
                      <!-- ThresholdBar for usage visualization -->
                      {#if q.limit > 0}
                        <ThresholdBar
                          value={Math.round((q.recent / q.limit) * 100)}
                          label="{q.recent}/{q.limit}"
                          suffix="%"
                          thresholds={[50, 80]}
                          color="auto"
                        />
                      {/if}
                      <div class="flex items-center justify-between text-[11px]">
                        <span class="text-[var(--fp-dim)]">
                          Reset: {formatLocalDate(q.reset_at_utc) || q.reset_at}
                          {#if q.resets_in}
                            <span class="ml-1 opacity-75">({q.resets_in})</span>
                          {/if}
                        </span>
                        {#if q.has_entitlement}
                          <span class="text-[var(--fp-teal)]">Entitled: {q.entitled}</span>
                        {/if}
                      </div>
                    </div>
                  {/each}
                </div>
              </div>
            {:else}
              <p class="text-xs text-[var(--fp-dim)] italic">No quota data available for this session.</p>
            {/if}

            <!-- Test result inline (if tested) -->
            {#if tr}
              <div class="flex items-center gap-3 p-3 rounded-lg fp-inset text-xs font-mono">
                <StatusBadge variant={tr.ok ? 'teal' : 'red'}>
                  {tr.ok ? 'pass' : 'fail'}
                </StatusBadge>
                <span class="text-[var(--fp-muted)]">
                  Latency: <span class="text-white tabular-nums">{tr.latencyMs}ms</span>
                </span>
                <span class="text-[var(--fp-muted)]">
                  Model: <span class="text-white">{tr.model}</span>
                </span>
                <span class="text-[var(--fp-muted)]">
                  Instance: <span class="text-white">{tr.instance}</span>
                </span>
              </div>
            {/if}

            <!-- C: Test button (expanded) -->
            <div class="flex items-center gap-2">
              <button
                onclick={() => testToken(token.index ?? i)}
                class="fp-btn-secondary text-[var(--fp-teal)] border-[var(--fp-teal)]/30"
              >
                <Zap size={12} />
                <span>Test this token</span>
              </button>
            </div>

            <!-- D: Help text -->
            <p class="text-[11px] text-[var(--fp-dim)]">
              Pacific day resets at 07:00 UTC. Usage bars show consumption against session quotas.
            </p>
          </div>
        {/if}
      </div>
    {/each}

    {#if !loading && (!data?.tokens || data.tokens.length === 0)}
      <div class="fp-card p-8 text-center">
        <p class="text-[var(--fp-muted)] text-sm">No tokens in pool. Add one above or use the OAuth generator.</p>
      </div>
    {/if}
  </div>
</div>
