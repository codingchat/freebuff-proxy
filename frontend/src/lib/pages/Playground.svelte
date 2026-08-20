<script>
  import { onMount } from 'svelte';
  import { Play, Brain, XCircle, Wifi, WifiOff, Zap, Clock } from '@lucide/svelte';
  import PageHeader from '../components/PageHeader.svelte';
  import Alert from '../components/Alert.svelte';
  import CopyButton from '../components/CopyButton.svelte';
  import StatusBadge from '../components/StatusBadge.svelte';
  import { fetchAPI } from '../utils/api.js';

  // State
  let models = $state([]);
  let selectedModel = $state('');
  let prompt = $state('');
  let output = $state('');
  let reasoning = $state('');
  let streaming = $state(false);
  let errorMsg = $state('');
  let modelsError = $state('');
  let abortController = null;

  // New: connection, metrics, context
  let connected = $state(false);
  let messageCount = $state(0);
  let latencyMs = $state(0);
  let tokenCount = $state(0);
  let sendStartTime = $state(0);
  let lastResponseModel = $state('');
  let responseStatus = $state('idle'); // 'idle' | 'streaming' | 'success' | 'error'

  // Derived: connection status
  let connectionLabel = $derived(connected ? 'Connected' : 'Disconnected');
  let connectionHost = $derived(window.location.hostname || 'localhost');
  let responseLatency = $derived(latencyMs > 0 ? `${latencyMs}ms` : '--');
  let responseModel = $derived(lastResponseModel || selectedModel || '--');
  let tokenDisplay = $derived(tokenCount > 0 ? tokenCount.toString() : '--');

  // Context badges: derive model short name
  let modelShort = $derived.by(() => {
    if (!selectedModel) return '';
    const parts = selectedModel.split('/');
    return parts.length > 1 ? parts[parts.length - 1] : selectedModel;
  });

  async function fetchModels() {
    try {
      const data = await fetchAPI('/admin/api/models');
      models = data.models.map(m => m.id);
      modelsError = '';
      connected = true;
      if (models.length > 0 && !selectedModel) {
        const preferred = models.find(m => m === 'deepseek/deepseek-v4-flash') ||
                          models.find(m => m.includes('deepseek-v4-flash')) ||
                          models[0];
        selectedModel = preferred;
      }
    } catch {
      modelsError = "Couldn't load the model list. Check the server connection and retry, or refresh the page.";
      connected = false;
    }
  }

  async function sendPrompt(e) {
    e?.preventDefault();
    if (!prompt.trim() || streaming || !selectedModel) return;

    streaming = true;
    output = '';
    reasoning = '';
    errorMsg = '';
    responseStatus = 'streaming';
    sendStartTime = performance.now();
    messageCount += 1;

    abortController = new AbortController();

    try {
      const res = await fetch('/admin/playground/chat', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Accept': 'application/json', 'X-Requested-With': 'fetch' },
        body: JSON.stringify({ model: selectedModel, prompt: prompt.trim(), stream: true }),
        signal: abortController.signal,
      });

      if (!res.ok) {
        const errText = await res.text();
        errorMsg = `HTTP ${res.status}: ${errText}`;
        responseStatus = 'error';
        streaming = false;
        return;
      }

      lastResponseModel = selectedModel;
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });

        let idx;
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const chunk = buf.slice(0, idx);
          buf = buf.slice(idx + 2);

          for (const line of chunk.split('\n')) {
            if (!line.startsWith('data:')) continue;
            const data = line.slice(5).trim();
            if (data === '[DONE]') continue;
            try {
              const obj = JSON.parse(data);
              const delta = obj.choices?.[0]?.delta;
              if (delta) {
                if (delta.reasoning_content) reasoning += delta.reasoning_content;
                if (delta.content) output += delta.content;
              }
              // Track usage if present
              if (obj.usage) {
                tokenCount = obj.usage.total_tokens || 0;
              }
            } catch { /* buffered partial */ }
          }
        }
      }

      latencyMs = Math.round(performance.now() - sendStartTime);
      // Estimate token count from output length if no usage reported
      if (tokenCount === 0 && output.length > 0) {
        tokenCount = Math.ceil(output.length / 4);
      }
      responseStatus = output.length > 0 ? 'success' : 'idle';
    } catch (err) {
      if (err.name !== 'AbortError') {
        errorMsg = `Stream failed: ${err.message}`;
        responseStatus = 'error';
        latencyMs = Math.round(performance.now() - sendStartTime);
      } else {
        responseStatus = 'idle';
      }
    } finally {
      streaming = false;
      abortController = null;
    }
  }

  function handleKeydown(e) {
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      sendPrompt();
    }
  }

  function clearAll() {
    prompt = '';
    output = '';
    reasoning = '';
    errorMsg = '';
    responseStatus = 'idle';
    latencyMs = 0;
    tokenCount = 0;
    lastResponseModel = '';
  }

  function cancelStream() {
    if (abortController) {
      abortController.abort();
      abortController = null;
      streaming = false;
      responseStatus = output.length > 0 ? 'success' : 'idle';
    }
  }

  onMount(() => { fetchModels(); });
</script>

<div class="space-y-6 page-enter">
  <PageHeader title="Model Playground" subtitle="Interactive prompt console with live SSE streaming and reasoning inspection" />

  <!-- A→B→C→D: Connection Status -->
  <div class="fp-card p-4 flex items-center justify-between">
    <div class="flex items-center gap-3">
      {#if connected}
        <StatusBadge variant="teal">
          <Wifi size={12} />
          <span>{connectionLabel}</span>
        </StatusBadge>
      {:else}
        <StatusBadge variant="red">
          <WifiOff size={12} />
          <span>{connectionLabel}</span>
        </StatusBadge>
      {/if}
      {#if models.length > 0}
        <span class="text-xs text-[var(--fp-dim)] font-mono">{models.length} model{models.length !== 1 ? 's' : ''} available</span>
      {/if}
    </div>
    <span class="text-xs text-[var(--fp-dim)]">Connected to proxy at {connectionHost}</span>
  </div>

  <!-- A→B→C→D: Context Badges -->
  <div class="flex flex-wrap items-center gap-2">
    {#if modelShort}
      <StatusBadge variant="blue" mono>
        <Zap size={11} />
        <span>{modelShort}</span>
      </StatusBadge>
    {/if}
    {#if streaming}
      <StatusBadge variant="amber">
        <span class="w-1.5 h-1.5 rounded-full bg-[var(--fp-amber)] animate-pulse"></span>
        <span>Streaming</span>
      </StatusBadge>
    {:else if responseStatus === 'success'}
      <StatusBadge variant="teal">
        <span>Ready</span>
      </StatusBadge>
    {:else if responseStatus === 'error'}
      <StatusBadge variant="red">
        <span>Error</span>
      </StatusBadge>
    {:else}
      <StatusBadge variant="muted">
        <span>Idle</span>
      </StatusBadge>
    {/if}
    {#if messageCount > 0}
      <StatusBadge variant="muted" mono>
        <span>Messages: {messageCount}</span>
      </StatusBadge>
    {/if}
  </div>

  <!-- A→B→C→D: Prompt Form (Chat Area) -->
  <div class="fp-card p-5 space-y-4">
    <div class="flex flex-col sm:flex-row sm:items-center justify-between gap-3">
      <div class="flex items-center gap-2">
        <label for="pg-model-select" class="text-sm font-semibold text-white">Model:</label>
        <div class="flex items-center gap-1.5">
          <select
            id="pg-model-select"
            bind:value={selectedModel}
            class="fp-input fp-input-mono text-sm w-auto"
          >
            {#each models as m}
              <option value={m}>{m}</option>
            {/each}
          </select>
          <CopyButton text={selectedModel} variant="icon" />
          {#if modelsError}
            <button type="button" onclick={fetchModels} class="fp-btn-secondary text-xs px-2.5 py-1">Retry</button>
          {/if}
        </div>
      </div>
      <div class="flex items-center gap-3 text-xs text-[var(--fp-dim)] font-mono">
        <span>Messages: {messageCount}</span>
        <span>Press Ctrl+Enter to send</span>
      </div>
    </div>

    <label for="pg-prompt" class="sr-only">Prompt</label>
    <textarea
      id="pg-prompt"
      bind:value={prompt}
      onkeydown={handleKeydown}
      rows="5"
      placeholder="Ask the model anything (e.g. write an idiomatic Go concurrent worker pool)..."
      class="fp-input fp-input-mono text-sm p-3"
    ></textarea>

    <!-- Actions: Cancel, Clear, Copy, Send -->
    <div class="flex items-center justify-between gap-2">
      <span class="text-xs text-[var(--fp-dim)]">Test chat completions through the proxy</span>
      <div class="flex items-center gap-2">
        {#if output}
          <CopyButton text={output} variant="labeled" label="Copy Response" />
        {/if}
        <button type="button" onclick={clearAll} class="fp-btn-secondary">Clear</button>
        {#if streaming}
          <button type="button" onclick={cancelStream} class="fp-btn-danger">
            <XCircle size={14} />
            <span>Cancel</span>
          </button>
        {/if}
        <button
          type="button"
          onclick={sendPrompt}
          disabled={streaming || !prompt.trim() || !selectedModel}
          class="fp-btn-primary"
        >
          <Play size={16} />
          <span>{streaming ? 'Streaming...' : 'Send Prompt'}</span>
        </button>
      </div>
    </div>
  </div>

  <!-- Errors -->
  {#if modelsError}
    <Alert variant="error" message={modelsError} dismissable={true} ondismiss={() => (modelsError = '')} />
  {/if}
  {#if errorMsg}
    <Alert variant="error" message={errorMsg} dismissable={false} />
  {/if}

  <!-- Reasoning -->
  {#if reasoning}
    <details class="fp-card overflow-hidden" open>
      <summary class="p-3.5 bg-[var(--fp-input-bg)] cursor-pointer text-xs font-semibold text-[var(--fp-amber)] flex items-center justify-between select-none">
        <span class="flex items-center gap-2">
          <Brain size={14} />
          <span>Thinking Process / Reasoning ({reasoning.length} chars)</span>
        </span>
        <CopyButton text={reasoning} variant="inline" label="Copy" />
      </summary>
      <div class="p-4 text-xs font-mono text-[var(--fp-muted)] whitespace-pre-wrap leading-relaxed border-t border-[var(--fp-border)] max-h-96 overflow-y-auto">
        {reasoning}
      </div>
    </details>
  {/if}

  <!-- A→B→C→D: Response / Output -->
  <div class="fp-card p-5 min-h-[160px]">
    <!-- A: Status badge + B: Metadata row -->
    <div class="text-xs font-semibold text-[var(--fp-dim)] uppercase tracking-wider mb-2 flex items-center justify-between">
      <div class="flex items-center gap-2">
        <span>Output Stream</span>
        {#if streaming}
          <StatusBadge variant="amber">
            <span class="w-1.5 h-1.5 rounded-full bg-[var(--fp-amber)] animate-pulse"></span>
            <span>Streaming</span>
          </StatusBadge>
        {:else if responseStatus === 'success'}
          <StatusBadge variant="teal">Success</StatusBadge>
        {:else if responseStatus === 'error'}
          <StatusBadge variant="red">Error</StatusBadge>
        {/if}
      </div>

      <!-- B: Response metadata -->
      <div class="flex items-center gap-3 text-[var(--fp-dim)]">
        {#if latencyMs > 0}
          <span class="flex items-center gap-1">
            <Clock size={11} />
            <span>{responseLatency}</span>
          </span>
        {/if}
        {#if lastResponseModel}
          <span class="font-mono">{responseModel}</span>
        {/if}
        {#if tokenCount > 0}
          <span class="font-mono">{tokenDisplay} tokens</span>
        {/if}
      </div>
    </div>

    <!-- D: Help text -->
    <p class="text-[10px] text-[var(--fp-dim)] mb-3">Streaming responses shown in real-time</p>

    <!-- Output content -->
    <div class="text-sm font-mono text-[var(--fp-text)] whitespace-pre-wrap leading-relaxed">
      {#if output}
        {output}
      {:else if !streaming}
        <span class="text-[var(--fp-dim)]">// Model response will stream here in real-time...</span>
      {/if}
    </div>

    <!-- C: Copy response (bottom) -->
    {#if output && !streaming}
      <div class="mt-4 pt-3 border-t border-[var(--fp-border)] flex items-center justify-between">
        <span class="text-[10px] text-[var(--fp-dim)]">{output.length} chars</span>
        <CopyButton text={output} variant="labeled" label="Copy Response" />
      </div>
    {/if}
  </div>
</div>
