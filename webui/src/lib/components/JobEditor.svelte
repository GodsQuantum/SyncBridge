<script lang="ts">
  import type { ApiClient } from '../api/client';
  import type { Job } from '../domain/types';
  import { formToJobInput, jobToForm, newJobForm, setJobActionType, type JobForm } from '../domain/jobs';
  import { cronHuman } from '../domain/format';
  import { tr, type Lang } from '../i18n';
  import FolderPane from './FolderPane.svelte';

  export let client: ApiClient;
  export let current: Job | null = null;
  export let initial: JobForm | null = null;
  export let lang: Lang = 'en';
  export let onSave: (input: ReturnType<typeof formToJobInput>) => Promise<void> | void = () => {};
  export let onClose: () => void = () => {};

  let form: JobForm = current ? jobToForm(current) : initial ? structuredClone(initial) : newJobForm();
  let saving = false;
  let error = '';
  let advanced = false;
  const presets = ['*/15 * * * *','0 * * * *','0 3 * * *','0 3 * * 1-5','0 3 * * 0'];

  function setAction(type: 'sync'|'command'|'script') {
    form = setJobActionType(form, type);
  }
  function setScheduler(owner: 'syncbridge'|'system') {
    form.schedulerOwner = owner;
    if (owner === 'system' && form.trigger === 'manual') form.trigger = 'cron';
    form = { ...form };
  }
  function setTrigger(trigger: 'manual'|'cron'|'watch') { form.trigger = trigger; form = { ...form }; }
  function pickSource(path: string) { if (form.action.type === 'sync' && form.action.sync) form.action.sync.source = path; else form.source = path; form = { ...form }; }
  function pickDest(path: string) { if (form.action.type === 'sync' && form.action.sync) form.action.sync.dest = path; form = { ...form }; }
  async function submit() {
    saving = true; error = '';
    try {
      if (!form.name.trim()) throw new Error(tr(lang,'fieldNameRequired'));
      if (form.action.type === 'sync' && (!form.action.sync?.source || !form.action.sync?.dest)) throw new Error(tr(lang,'fieldPathsRequired'));
      if (form.action.type === 'command' && !form.action.command?.trim()) throw new Error(tr(lang,'fieldCommandRequired'));
      if (form.action.type === 'script' && !form.action.scriptPath?.startsWith('/')) throw new Error(tr(lang,'fieldScriptAbsolute'));
      if (form.schedulerOwner === 'system' && form.trigger === 'manual') throw new Error(tr(lang,'fieldSystemTrigger'));
      await onSave(formToJobInput(form));
    } catch (e) { error = e instanceof Error ? e.message : String(e); saving = false; }
  }
</script>

<div class="scrim open" role="presentation" on:click|self={onClose}>
  <div class="modal" role="dialog" aria-modal="true" aria-labelledby="editor-title">
    <header class="modal-h"><h2 id="editor-title">{current ? tr(lang,'editJob') : tr(lang,'newJob')}</h2><span class="flex"></span><button class="btn ghost sm icon-btn" aria-label="Close" on:click={onClose}>✕</button></header>
    <div class="modal-b">
      {#if error}<div class="error-banner">{error}</div>{/if}
      <div class="field"><div class="field-label">{tr(lang,'name')}</div><input bind:value={form.name} type="text" placeholder="Production → nightly backup"></div>

      <div class="field"><div class="field-label">{tr(lang,'jobType')}</div><div class="cards">
        <button type="button" class="card" class:sel={form.action.type==='sync'} on:click={() => setAction('sync')}><span class="ck"></span><div class="ct">{tr(lang,'synchronization')}</div><div class="cd">{tr(lang,'syncDesc')}</div></button>
        <button type="button" class="card" class:sel={form.action.type==='command'} on:click={() => setAction('command')}><span class="ck"></span><div class="ct">{tr(lang,'command')}</div><div class="cd">{tr(lang,'commandDesc')}</div></button>
        <button type="button" class="card" class:sel={form.action.type==='script'} on:click={() => setAction('script')}><span class="ck"></span><div class="ct">{tr(lang,'script')}</div><div class="cd">{tr(lang,'scriptDesc')}</div></button>
      </div></div>

      {#if form.action.type === 'command'}
        <div class="field"><div class="field-label">{tr(lang,'command')}</div><input bind:value={form.action.command} type="text" placeholder="docker system prune -f"><div class="hint">{tr(lang,'commandHint')}</div></div>
      {:else if form.action.type === 'script'}
        <div class="field"><div class="field-label">{tr(lang,'scriptPath')}</div><input bind:value={form.action.scriptPath} type="text" placeholder="/srv/scripts/maintenance.sh"></div>
        <div class="field"><div class="field-label">{tr(lang,'arguments')}</div><input value={(form.action.scriptArgs ?? []).join(' ')} on:input={(e) => { form.action.scriptArgs=(e.currentTarget as HTMLInputElement).value.split(/\s+/).filter(Boolean); form={...form}; }} type="text" placeholder="--safe --verbose"></div>
      {/if}

      <div class="field"><div class="field-label">{tr(lang,'executionOwnership')}</div><div class="cards">
        <button type="button" class="card" class:sel={form.schedulerOwner==='syncbridge'} on:click={() => setScheduler('syncbridge')}><span class="ck"></span><div class="ct">{tr(lang,'syncbridgeBackend')}</div><div class="cd">{tr(lang,'syncbridgeBackendDesc')}</div></button>
        <button type="button" class="card" class:sel={form.schedulerOwner==='system'} on:click={() => setScheduler('system')}><span class="ck"></span><div class="ct">{tr(lang,'systemBackend')}</div><div class="cd">{tr(lang,'systemBackendDesc')}</div></button>
      </div></div>

      <div class="field"><div class="field-label">{tr(lang,'hostIdentity')}</div>
        {#if form.action.type === 'script'}<div class="inline-actions" style="margin-bottom:10px"><button type="button" class="tab" class:sel={form.identity.mode==='script-owner'} on:click={() => {form.identity.mode='script-owner';form = { ...form }; }}>{tr(lang,'scriptOwner')}</button><button type="button" class="tab" class:sel={form.identity.mode==='fixed'} on:click={() => {form.identity.mode='fixed';form = { ...form }; }}>{tr(lang,'fixedIdentity')}</button></div>{/if}
        {#if form.identity.mode === 'fixed'}<div class="grid2"><div class="field"><div class="field-label">{tr(lang,'user')}</div><input bind:value={form.identity.user} type="text" placeholder="operator"></div><div class="field"><div class="field-label">UID</div><input bind:value={form.identity.uid} type="number" min="0"></div><div class="field"><div class="field-label">{tr(lang,'group')}</div><input bind:value={form.identity.group} type="text" placeholder="operator"></div><div class="field"><div class="field-label">GID</div><input bind:value={form.identity.gid} type="number" min="0"></div></div>{/if}
      </div>

      {#if form.action.type === 'sync' && form.action.sync}
        <div class="section-h">{tr(lang,'folders')}</div><div class="dual"><FolderPane {client} label={tr(lang,'sourceA')} lang={lang} accent="src" value={form.action.sync.source ?? ''} onPick={pickSource}/><div class="arrow-col">→</div><FolderPane {client} label={tr(lang,'destinationB')} lang={lang} accent="dst" value={form.action.sync.dest ?? ''} onPick={pickDest}/></div>
        <div class="field"><div class="field-label">{tr(lang,'copyBehavior')}</div><div class="cards">
          {#each [{id:'add',t:tr(lang,'accumulation'),d:tr(lang,'accumulationDesc')},{id:'mirror',t:tr(lang,'exactMirror'),d:tr(lang,'exactMirrorDesc')},{id:'move',t:tr(lang,'move'),d:tr(lang,'moveDesc')}] as item}
            <button type="button" class="card {item.id}" class:sel={form.action.sync?.mode===item.id} on:click={() => { if(form.action.sync) form.action.sync.mode=item.id; form={...form}; }}><span class="ck"></span><div class="ct">{item.t}</div><div class="cd">{item.d}</div></button>
          {/each}
        </div></div>
        <div class="grid2"><div class="field"><div class="field-label">{tr(lang,'engine')}</div><select bind:value={form.action.sync.engine}><option value="rsync">rsync</option><option value="rclone">rclone</option></select></div><div class="field"><div class="field-label">{tr(lang,'compare')}</div><select bind:value={form.action.sync.compare}><option value="time">time + size</option><option value="checksum">checksum</option></select></div></div>
      {/if}

      <div class="field"><div class="field-label">{tr(lang,'trigger')}</div><div class="cards">
        <button type="button" class="card" class:sel={form.trigger==='manual'} disabled={form.schedulerOwner==='system'} on:click={() => setTrigger('manual')}><span class="ck"></span><div class="ct">{tr(lang,'manual')}</div><div class="cd">{tr(lang,'manualDesc')}</div></button>
        <button type="button" class="card" class:sel={form.trigger==='cron'} on:click={() => setTrigger('cron')}><span class="ck"></span><div class="ct">{tr(lang,'scheduled')}</div><div class="cd">{tr(lang,'scheduledDesc')}</div></button>
        <button type="button" class="card" class:sel={form.trigger==='watch'} on:click={() => setTrigger('watch')}><span class="ck"></span><div class="ct">{tr(lang,'watch')}</div><div class="cd">{tr(lang,'watchDesc')}</div></button>
      </div></div>
      {#if form.trigger === 'cron'}<div class="field"><div class="field-label">Cron</div><div class="cron-presets">{#each presets as preset}<button type="button" class="cron-chip" class:sel={form.cron===preset} on:click={() => {form.cron=preset;form = { ...form }; }}>{preset}</button>{/each}</div><input bind:value={form.cron} type="text" placeholder="0 3 * * *" style="margin-top:10px"><div class="cron-live">↳ {cronHuman(form.cron, lang)}</div></div>{/if}
      {#if form.trigger === 'watch'}
        {#if form.action.type !== 'sync'}<div class="field"><div class="field-label">{tr(lang,'folderToWatch')}</div><input bind:value={form.source} type="text" placeholder="/mnt/inbox"></div>{/if}
        <div class="grid2"><div class="field"><div class="field-label">{tr(lang,'filePatterns')}</div><input bind:value={form.watchGlob} type="text" placeholder="*.mp4, *.m4a"></div><div class="field"><div class="field-label">{tr(lang,'watchMode')}</div><select bind:value={form.watchMode}><option value="hybrid">hybrid</option><option value="event">events</option><option value="poll">poll</option></select></div><div class="field"><div class="field-label">{tr(lang,'debounce')}</div><input bind:value={form.debounce} type="number" min="0"></div><div class="field"><div class="field-label">{tr(lang,'pollInterval')}</div><input bind:value={form.pollSec} type="number" min="1"></div></div>
      {/if}

      <button type="button" class="adv-toggle" class:open={advanced} on:click={() => advanced=!advanced}><span class="chev">›</span> {tr(lang,'advanced')}</button>
      {#if advanced}<div class="adv open">
        <div class="grid2"><div class="field"><div class="field-label">{tr(lang,'timeout')}</div><input bind:value={form.timeoutSeconds} type="number" min="0"></div><div class="field"><div class="field-label">{tr(lang,'stopGrace')}</div><input bind:value={form.stopGraceSeconds} type="number" min="0"></div><div class="field"><div class="field-label">{tr(lang,'overlap')}</div><select bind:value={form.overlap}><option value="skip">skip</option><option value="queue-latest">queue latest</option></select></div><div class="field"><div class="field-label">{tr(lang,'umask')}</div><input bind:value={form.umask} type="number" min="0" max="511"></div></div>
        <div class="field"><div class="field-label">{tr(lang,'environment')}</div><input value={form.environment.join(', ')} on:input={(e)=>{form.environment=(e.currentTarget as HTMLInputElement).value.split(',').map(x=>x.trim()).filter(Boolean);form = { ...form }; }} type="text" placeholder="KEY=value, OTHER=value"></div>
        {#if form.action.type === 'sync' && form.action.sync}<div class="grid2"><div class="field"><div class="field-label">{tr(lang,'bandwidth')}</div><input bind:value={form.action.sync.bwlimit} type="text" placeholder="50M"></div><div class="field"><div class="field-label">{tr(lang,'exclude')}</div><input bind:value={form.action.sync.exclude} type="text" placeholder="*.tmp, cache/**"></div><div class="field"><div class="field-label">{tr(lang,'maxDelete')}</div><input bind:value={form.action.sync.maxDel} type="number" min="0"></div><div class="field"><div class="field-label">{tr(lang,'backupKeep')}</div><input bind:value={form.action.sync.backupKeep} type="number" min="0"></div></div><div class="inline-actions"><label><input bind:checked={form.action.sync.backup} type="checkbox"> {tr(lang,'backupChanged')}</label><label><input bind:checked={form.action.sync.skipNew} type="checkbox"> {tr(lang,'skipNewer')}</label><label><input bind:checked={form.action.sync.sysBackup} type="checkbox"> {tr(lang,'systemBackup')}</label></div>{/if}
      </div>{/if}
    </div>
    <footer class="modal-f"><button class="btn ghost" on:click={onClose}>{tr(lang,'cancel')}</button><button class="btn pri" disabled={saving} on:click={submit}>{saving ? tr(lang,'saving') : tr(lang,'saveJob')}</button></footer>
  </div>
</div>
