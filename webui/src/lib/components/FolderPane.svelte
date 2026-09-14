<script lang="ts">
  import { onMount } from 'svelte';
  import type { ApiClient } from '../api/client';
  import { browse } from '../api/syncbridge';
  import { tr, type Lang } from '../i18n';
  export let client: ApiClient;
  export let label = 'Source';
  export let accent: 'src' | 'dst' = 'src';
  export let value = '';
  export let lang: Lang = 'en';
  export let onPick: (path: string) => void = () => {};

  let current = value || '/mnt';
  let parent = '';
  let dirs: string[] = [];
  let busy = false;
  let error = '';
  $: if (value && !current) current = value;

  async function load(path: string) {
    busy = true; error = '';
    try {
      const result = await browse(client, path);
      current = result.path; parent = result.parent; dirs = result.dirs;
    } catch (e) { error = e instanceof Error ? e.message : String(e); }
    finally { busy = false; }
  }
  function child(name: string) { return current === '/' ? `/${name}` : `${current.replace(/\/$/, '')}/${name}`; }
  onMount(() => { void load(current); });
</script>

<div class="pane">
  <div class="pane-h"><span class="ic">◆</span><span class="lab {accent}">{label}</span></div>
  <div class="pane-cur" title={current}>{current}</div>
  <div class="pane-list">
    {#if !dirs.length && !busy}<button type="button" class="pane-item" on:click={() => load(current)}>↻ {tr(lang,'browse')} {current}</button>{/if}
    {#if parent}<button type="button" class="pane-item up" on:click={() => load(parent)}><span class="ic">↰</span>..</button>{/if}
    {#each dirs as dir}
      <button type="button" class="pane-item" class:dst-i={accent === 'dst'} on:click={() => load(child(dir))}><span class="ic">◆</span>{dir}</button>
    {/each}
    {#if busy}<div class="pane-item dim">{tr(lang,'loading')}</div>{/if}
    {#if error}<div class="pane-item danger">{error}</div>{/if}
  </div>
  <div class="pane-pick">
    <span class="cur-mini">{value || '—'}</span>
    <button type="button" class="btn sm pri" on:click={() => onPick(current)}>{tr(lang,'choose')}</button>
  </div>
</div>
