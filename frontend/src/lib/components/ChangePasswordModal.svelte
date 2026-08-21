<script>
  import { X, Lock, Key, AlertTriangle, Check } from '@lucide/svelte';
  import Button from './Button.svelte';
  import Alert from './Alert.svelte';
  import Field from './Field.svelte';
  import { postAPI } from '../api/client.js';

  /**
   * @prop {boolean} open
   * @prop {() => void} [onSuccess]
   * @prop {() => void} [onClose]
   */
  let { open = $bindable(false), onSuccess, onClose } = $props();

  let currentPassword = $state('');
  let newPassword = $state('');
  let confirmPassword = $state('');
  let submitting = $state(false);
  let errorMsg = $state('');
  let successMsg = $state('');

  function resetForm() {
    currentPassword = '';
    newPassword = '';
    confirmPassword = '';
    errorMsg = '';
    successMsg = '';
  }

  function handleClose() {
    resetForm();
    open = false;
    onClose?.();
  }

  async function handleSubmit(e) {
    e.preventDefault();
    errorMsg = '';
    successMsg = '';

    if (!currentPassword.trim()) {
      errorMsg = 'Please enter your current password.';
      return;
    }
    if (newPassword.length < 6) {
      errorMsg = 'New password must be at least 6 characters.';
      return;
    }
    if (newPassword === '123456') {
      errorMsg = 'New password cannot be the default password (123456).';
      return;
    }
    if (newPassword !== confirmPassword) {
      errorMsg = 'New passwords do not match.';
      return;
    }

    submitting = true;
    try {
      const res = await postAPI('/admin/api/change-password', {
        current_password: currentPassword.trim(),
        new_password: newPassword.trim(),
      });

      if (res.ok) {
        successMsg = res.message || 'Admin password updated successfully!';
        onSuccess?.();
        setTimeout(() => {
          handleClose();
        }, 1200);
      } else {
        errorMsg = res.message || 'Failed to update password.';
      }
    } catch (err) {
      errorMsg = err.message || 'Could not update password. Check connection.';
    } finally {
      submitting = false;
    }
  }
</script>

{#if open}
  <div class="fixed inset-0 z-50 flex items-center justify-center p-4">
    <!-- Backdrop -->
    <button
      type="button"
      class="fixed inset-0 bg-black/75 backdrop-blur-sm transition-opacity border-0 p-0 m-0 w-full h-full cursor-default"
      onclick={handleClose}
      aria-label="Close dialog backdrop"
    ></button>

    <!-- Modal Card -->
    <div
      class="relative w-full max-w-md bg-[var(--fp-card)] border border-[var(--fp-border)] rounded-xl shadow-2xl p-6 z-10 space-y-5 page-enter"
      role="dialog"
      aria-modal="true"
      aria-labelledby="modal-title"
    >
      <!-- Header -->
      <div class="flex items-center justify-between border-b border-[var(--fp-border)] pb-4">
        <div class="flex items-center gap-2.5">
          <div class="w-8 h-8 rounded-lg bg-[var(--fp-accent)]/10 text-[var(--fp-accent)] flex items-center justify-center">
            <Lock size={18} />
          </div>
          <div>
            <h2 id="modal-title" class="text-sm font-semibold text-[var(--fp-text)]">Change Admin Password</h2>
            <p class="text-xs text-[var(--fp-muted)]">Update the master administrative password</p>
          </div>
        </div>
        <button
          type="button"
          onclick={handleClose}
          class="text-[var(--fp-dim)] hover:text-[var(--fp-text)] p-1 rounded-lg hover:bg-[var(--fp-border)]/50 transition-colors"
          aria-label="Close modal"
        >
          <X size={16} />
        </button>
      </div>

      <!-- Messages -->
      {#if errorMsg}
        <Alert tone="error">{errorMsg}</Alert>
      {/if}
      {#if successMsg}
        <Alert tone="success">{successMsg}</Alert>
      {/if}

      <!-- Form -->
      <form onsubmit={handleSubmit} class="space-y-4">
        <Field label="Current Password" id="current-password">
          <input
            id="current-password"
            type="password"
            bind:value={currentPassword}
            placeholder="Enter current password (default: 123456)"
            required
            class="w-full px-3 py-2 bg-[var(--fp-bg)] border border-[var(--fp-border)] rounded-lg text-sm text-[var(--fp-text)] placeholder-[var(--fp-dim)] focus:outline-none focus:border-[var(--fp-accent)] font-mono"
          />
        </Field>

        <Field label="New Password" id="new-password" hint="Minimum 6 characters">
          <input
            id="new-password"
            type="password"
            bind:value={newPassword}
            placeholder="Enter secure new password"
            required
            minlength="6"
            class="w-full px-3 py-2 bg-[var(--fp-bg)] border border-[var(--fp-border)] rounded-lg text-sm text-[var(--fp-text)] placeholder-[var(--fp-dim)] focus:outline-none focus:border-[var(--fp-accent)] font-mono"
          />
        </Field>

        <Field label="Confirm New Password" id="confirm-password">
          <input
            id="confirm-password"
            type="password"
            bind:value={confirmPassword}
            placeholder="Re-enter new password"
            required
            minlength="6"
            class="w-full px-3 py-2 bg-[var(--fp-bg)] border border-[var(--fp-border)] rounded-lg text-sm text-[var(--fp-text)] placeholder-[var(--fp-dim)] focus:outline-none focus:border-[var(--fp-accent)] font-mono"
          />
        </Field>

        <div class="flex items-center justify-end gap-3 pt-3 border-t border-[var(--fp-border)]">
          <Button variant="ghost" onclick={handleClose} disabled={submitting}>
            Cancel
          </Button>
          <Button variant="primary" type="submit" loading={submitting}>
            <Key size={14} />
            Update Password
          </Button>
        </div>
      </form>
    </div>
  </div>
{/if}
