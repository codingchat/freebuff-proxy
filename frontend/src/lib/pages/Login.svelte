<script>
  import { Lock } from '@lucide/svelte';
  import Alert from '../components/Alert.svelte';

  let token = $state('');
  let errorMsg = $state('');
  let loading = $state(false);

  const GENERIC_LOGIN_ERROR = 'Invalid password.';

  // The server replies to failed logins with {"error":"..."} JSON, but a
  // proxy or error page in front of it can return HTML or an empty body.
  // Only surface a clean message; never dump the raw response body.
  function cleanLoginError(body) {
    if (!body) return GENERIC_LOGIN_ERROR;
    try {
      const data = JSON.parse(body);
      const err = data?.error;
      const msg = typeof err === 'string' ? err : err?.message;
      if (typeof msg !== 'string' || !msg.trim()) return GENERIC_LOGIN_ERROR;
      return msg.trim();
    } catch {
      return GENERIC_LOGIN_ERROR;
    }
  }

  async function handleLogin(e) {
    e.preventDefault();
    if (!token.trim() || loading) return;
    loading = true;
    errorMsg = '';

    try {
      const res = await fetch('/admin/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ token: token.trim() }),
      });

      if (res.ok || res.redirected) {
        window.location.href = '/admin';
      } else {
        errorMsg = cleanLoginError(await res.text());
      }
    } catch (e) {
      errorMsg = 'Could not reach the server. Check the connection and try again.';
    } finally {
      loading = false;
    }
  }
</script>

<div class="min-h-[80vh] flex items-center justify-center px-4">
  <div class="max-w-md w-full fp-card p-8 space-y-6" style="box-shadow: var(--fp-shadow-lg);">
    <div class="text-center space-y-2">
      <div class="w-12 h-12 rounded-xl bg-[var(--fp-amber)]/10 border border-[var(--fp-amber)]/30 text-[var(--fp-amber)] flex items-center justify-center mx-auto mb-3">
        <Lock size={24} />
      </div>
      <h1 class="text-2xl font-bold text-white tracking-tight">freebuff-proxy</h1>
      <p class="text-xs text-[var(--fp-muted)]">Sign in to manage your proxy</p>
    </div>

    <Alert variant="error" message={errorMsg} dismissable={false} />

    <form onsubmit={handleLogin} class="space-y-4">
      <div>
        <label for="admin-token" class="block text-xs font-semibold text-[var(--fp-muted)] uppercase tracking-wider mb-2">Password</label>
        <input
          id="admin-token"
          type="password"
          bind:value={token}
          required
          autocomplete="current-password"
          placeholder="Enter password..."
          class="fp-input fp-input-mono"
        />
      </div>

      <button
        type="submit"
        disabled={loading || !token.trim()}
        class="w-full fp-btn-primary py-2.5"
      >
        <span>{loading ? 'Authenticating...' : 'Sign In'}</span>
      </button>
    </form>
  </div>
</div>
