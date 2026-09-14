<script lang="ts">
  import type { ApiClient } from '../api/client';
  import { addRemote, removeRemote } from '../api/syncbridge';
  import type { RemoteInstance } from '../domain/types';
  import { tr, type Lang } from '../i18n';
  export let client: ApiClient;
  export let lang: Lang = 'en';
  export let items: RemoteInstance[] = [];
  export let onReload: () => Promise<void> | void = () => {};
  export let onClose: () => void = () => {};
  let name='', url='', user='', password='', busy=false, error='';
  async function add(){busy=true;error='';try{await addRemote(client,{name,url,user,password});name=url=user=password='';await onReload();}catch(e){error=e instanceof Error?e.message:String(e)}finally{busy=false}}
  async function del(id:number){if(!confirm(tr(lang,'removeRemoteConfirm')))return;try{await removeRemote(client,id);await onReload();}catch(e){error=e instanceof Error?e.message:String(e)}}
</script>
<div class="scrim open" role="presentation" on:click|self={onClose}><section class="modal" role="dialog" aria-modal="true">
<header class="modal-h"><h2>{tr(lang,'remoteInstances')}</h2><span class="flex"></span><button class="btn ghost sm icon-btn" on:click={onClose}>✕</button></header>
<div class="modal-b">{#if error}<div class="error-banner">{error}</div>{/if}
{#if !items.length}<div class="hint" style="margin-bottom:18px">{tr(lang,'noRemote')}</div>{/if}
{#each items as item}<div class="rm-row"><span class="rm-name">{item.name}</span><span class="rm-url">{item.url}</span><span class="flex"></span><button class="btn sm dgr" on:click={()=>del(item.id)}>✕</button></div>{/each}
<div class="section-h" style="margin-top:22px">{tr(lang,'addInstance')}</div><div class="grid2"><div class="field"><div class="field-label">{tr(lang,'name')}</div><input bind:value={name} type="text" placeholder="NAS B"></div><div class="field"><div class="field-label">{tr(lang,'url')}</div><input bind:value={url} type="text" placeholder="https://sync.example.net"></div><div class="field"><div class="field-label">{tr(lang,'user')}</div><input bind:value={user} type="text" autocomplete="username"></div><div class="field"><div class="field-label">{tr(lang,'password')}</div><input bind:value={password} type="password" autocomplete="new-password"></div></div>
<div class="hint">{tr(lang,'remoteHint')}</div></div>
<footer class="modal-f"><button class="btn ghost" on:click={onClose}>{tr(lang,'close')}</button><button class="btn pri" disabled={busy||!name.trim()||!url.trim()} on:click={add}>{busy?tr(lang,'testing'):tr(lang,'addInstance')}</button></footer>
</section></div>
