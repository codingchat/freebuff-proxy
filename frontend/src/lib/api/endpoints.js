import { fetchAPI, postAPI, postForm } from './client.js';

/**
 * Overview & Diagnostics
 */
export async function getOverview() {
  return fetchAPI('/admin/api/overview');
}

export async function runSmokeTest(model, prompt = 'ping') {
  return postAPI('/admin/smoke', { model, prompt });
}

export async function runDiag() {
  return fetchAPI('/admin/diag');
}

/**
 * Token & Pool Management
 */
export async function getTokens() {
  return fetchAPI('/admin/tokens');
}

export async function addToken(token) {
  return postAPI('/admin/tokens/add', { token });
}

export async function removeToken(token) {
  return postAPI('/admin/tokens/remove', { token });
}

export async function clearCooldown(token) {
  return postAPI('/admin/tokens/clear_cooldown', { token });
}

export async function setPoolMode(mode) {
  return postAPI('/admin/mode', { mode });
}

/**
 * Config & Environment
 */
export async function getConfig() {
  return fetchAPI('/admin/config');
}

export async function saveConfig(envContent) {
  return postForm('/admin/config', { env: envContent });
}

export async function reloadConfig() {
  return postAPI('/admin/reload', {});
}

/**
 * Logs & Metrics
 */
export async function getLogs(level = '', filter = '', limit = 100) {
  const params = new URLSearchParams();
  if (level) params.set('level', level);
  if (filter) params.set('filter', filter);
  if (limit) params.set('limit', String(limit));
  return fetchAPI(`/admin/api/logs?${params.toString()}`);
}

export async function getMetrics() {
  return fetchAPI('/admin/api/metrics');
}

export async function getModels() {
  return fetchAPI('/admin/api/models');
}
