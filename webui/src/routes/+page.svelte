<script lang="ts">
  import { onMount } from 'svelte';
  import { ApiClient } from '$lib/api/client';
  import { capabilities, createJob, deleteJob, getSettings, history, jobs as getJobs, remotes as getRemotes, startRun, stopRun, updateJob } from '$lib/api/syncbridge';
  import { cloneJobInput, formToJobInput, jobToForm, type JobForm } from '$lib/domain/jobs';
  import { initialRunState, reduceRunEvent, type RunState } from '$lib/domain/runs';
  import type { CapabilityReport, Job, RemoteInstance, RunEvent, RunRecord, RunSnapshot, Settings } from '$lib/domain/types';
  import { tr, type Lang } from '$lib/i18n';
  import JobCard from '$lib/components/JobCard.svelte';
  import JobEditor from '$lib/components/JobEditor.svelte';
  import RemotesModal from '$lib/components/RemotesModal.svelte';
  import SettingsModal from '$lib/components/SettingsModal.svelte';
  import SystemImportModal from '$lib/components/SystemImportModal.svelte';

  let lang: Lang = 'en';
  let remotePrefix = '';
  let client = new ApiClient();
  let jobList: Job[] = [];
  let caps: CapabilityReport = { results: [] };
  let runState: RunState = initialRunState();
  let remoteList: RemoteInstance[] = [];
  let remoteStatus: Record<number,'on'|'off'> = {};
  let settings: Settings = { notifyUrl:'', notifyAll:false };
  let loading = true, error = '', query = '', filter = 'all';
  let editorOpen=false, editorJob:Job|null=null, editorInitial:JobForm|null=null;
  let importOpen=false, remotesOpen=false, settingsOpen=false;
  let openHistory: Record<number,boolean> = {}, histories: Record<number,RunRecord[]> = {};
  let toastMessage='', toastType=''; let toastTimer: ReturnType<typeof setTimeout>;
  let events: EventSource | null = null;

  $: rsync = caps.results.find(x=>x.code==='rsync')?.status === 'available';
  $: rclone = caps.results.find(x=>x.code==='rclone')?.status === 'available';
  $: visible = jobList.filter(job => {
    const q=query.trim().toLowerCase();
    const text=[job.name,job.action.type,job.action.command,job.action.scriptPath,job.action.sync?.source,job.action.sync?.dest,job.action.sync?.engine,job.trigger].filter(Boolean).join(' ').toLowerCase();
    if(q&&!text.includes(q))return false;
    if(filter==='sync'&&job.action.type!=='sync')return false;
    if(filter==='commands'&&job.action.type==='sync')return false;
    if(filter==='cron'&&job.trigger!=='cron')return false;
    if(filter==='watch'&&job.trigger!=='watch')return false;
    if(filter==='disabled'&&job.enabled)return false;
    return true;
  });
  $: counts={all:jobList.length,sync:jobList.filter(j=>j.action.type==='sync').length,commands:jobList.filter(j=>j.action.type!=='sync').length,cron:jobList.filter(j=>j.trigger==='cron').length,watch:jobList.filter(j=>j.trigger==='watch').length,disabled:jobList.filter(j=>!j.enabled).length};

  function toast(message:string,type='ok'){toastMessage=message;toastType=type;clearTimeout(toastTimer);toastTimer=setTimeout(()=>toastMessage='',2800)}
  async function loadCore(){events?.close();events=null;loading=true;error='';client=new ApiClient(remotePrefix);try{const [j,c]=await Promise.all([getJobs(client),capabilities(client)]);jobList=j;caps=c;runState=initialRunState();const latest=await Promise.all(j.map(job=>client.request<RunSnapshot[]>(`/api/v1/jobs/${job.id}/runs?limit=1`).catch(()=>[])));for(const list of latest){if(list[0]){runState.byJob[list[0].jobId]=list[0];runState.byId[list[0].id]=list[0]}}connectEvents();}catch(e){error=e instanceof Error?e.message:String(e)}finally{loading=false}}
  function connectEvents(){events?.close();events=new EventSource(client.path('/api/v1/events'));const consume=(event:MessageEvent)=>{try{runState=reduceRunEvent(runState,JSON.parse(event.data) as RunEvent)}catch{}};events.addEventListener('state',consume as EventListener);events.addEventListener('log',consume as EventListener);events.onerror=()=>{};}
  async function loadRemotes(){const local=new ApiClient();try{remoteList=await getRemotes(local)}catch{remoteList=[]}if(remotePrefix&&!remoteList.some(remote=>`/api/remote/${remote.id}`===remotePrefix)){remotePrefix='';document.body.classList.remove('piloting');await loadCore();toast(tr(lang,'remoteRemoved'))}remoteStatus={};await Promise.all(remoteList.map(async remote=>{try{await capabilities(new ApiClient(`/api/remote/${remote.id}`));remoteStatus={...remoteStatus,[remote.id]:'on'}}catch{remoteStatus={...remoteStatus,[remote.id]:'off'}}}))}
  async function changeInstance(prefix:string){remotePrefix=prefix;document.body.classList.toggle('piloting',!!prefix);await loadCore();toast(prefix?tr(lang,'pilotingRemote'):tr(lang,'localInstance'))}
  async function doRun(job:Job,dry:boolean){try{const {run}=await startRun(client,job,dry);runState={...runState,byJob:{...runState.byJob,[job.id]:run},byId:{...runState.byId,[run.id]:run}};toast(dry?tr(lang,'dryStarted'):tr(lang,'runStarted'))}catch(e){toast(e instanceof Error?e.message:String(e),'err')}}
  async function doStop(run:RunSnapshot){if(!confirm(tr(lang,'stopConfirm')))return;try{await stopRun(client,run.id);toast(tr(lang,'stopRequested'))}catch(e){toast(e instanceof Error?e.message:String(e),'err')}}
  function openNew(initial:JobForm|null=null){editorJob=null;editorInitial=initial;editorOpen=true;importOpen=false}
  function openEdit(job:Job){editorInitial=null;editorJob=job;editorOpen=true}
  async function saveJob(input:ReturnType<typeof formToJobInput>){try{if(editorJob)await updateJob(client,editorJob,input);else await createJob(client,input);editorOpen=false;await loadCore();toast(editorJob?tr(lang,'jobUpdated'):tr(lang,'jobCreated'))}catch(e){throw e}}
  async function clone(job:Job){try{await createJob(client,cloneJobInput(job));await loadCore();toast(tr(lang,'disabledCopy'))}catch(e){toast(e instanceof Error?e.message:String(e),'err')}}
  async function toggle(job:Job){try{const form=jobToForm(job);form.enabled=!job.enabled;await updateJob(client,job,formToJobInput(form));await loadCore();toast(form.enabled?tr(lang,'jobEnabled'):tr(lang,'jobDisabled'))}catch(e){toast(e instanceof Error?e.message:String(e),'err')}}
  async function remove(job:Job){if(!confirm(`${tr(lang,'deleteConfirm')} “${job.name}”`))return;try{await deleteJob(client,job);await loadCore();toast(tr(lang,'jobDeleted'))}catch(e){toast(e instanceof Error?e.message:String(e),'err')}}
  async function toggleHistory(job:Job){const open=!openHistory[job.id];openHistory={...openHistory,[job.id]:open};if(open&&!histories[job.id])try{histories={...histories,[job.id]:await history(client,job.id)}}catch(e){toast(e instanceof Error?e.message:String(e),'err')}}
  async function openSettings(){try{settings=await getSettings(new ApiClient())}catch{}settingsOpen=true}
  async function logout(){try{await fetch('/api/auth/logout',{method:'POST'})}finally{location.replace('/')}}
  function toggleLang(){lang=lang==='en'?'fr':'en';localStorage.setItem('sb_lang',lang);document.documentElement.lang=lang}

  onMount(()=>{lang=(localStorage.getItem('sb_lang') as Lang)||'en';document.documentElement.lang=lang;loadCore();loadRemotes();return()=>events?.close()});
</script>

<svelte:head><title>SyncBridge</title><meta name="description" content="Host-native sync and automation control plane"></svelte:head>
<div class="app">
  <header class="top">
    <div class="brand"><span class="mark"><i></i><i></i><i></i><i></i></span><span>SyncBridge<br><small>cron · watch · rsync · scripts</small></span></div>
    <span class="flex"></span>
    <div class="eng-pill"><b>engines</b><span class:on={rsync} class:off={!rsync}>rsync</span><span class:on={rclone} class:off={!rclone}>rclone</span></div>
    <div class="top-actions">
      <select class="inst-sel" bind:value={remotePrefix} on:change={(e)=>changeInstance((e.currentTarget as HTMLSelectElement).value)} aria-label="Piloted instance"><option value="">● {tr(lang,'local')}</option>{#each remoteList as remote}<option value={`/api/remote/${remote.id}`}>{remoteStatus[remote.id]==='on'?'●':'○'} {remote.name}</option>{/each}</select>
      <button class="btn ghost sm icon-btn" title={tr(lang,'remoteInstances')} on:click={()=>remotesOpen=true}>＋</button>
      <button class="btn ghost sm icon-btn" title={tr(lang,'settings')} on:click={openSettings}>⚙</button>
      <button class="btn ghost sm icon-btn" title={tr(lang,'logout')} on:click={logout}>⎋</button>
      <button class="btn ghost sm" on:click={toggleLang}>{lang==='en'?'FR':'EN'}</button>
      <button class="btn" on:click={()=>importOpen=true}>⤵ {tr(lang,'import')}</button>
      <button class="btn pri" on:click={()=>openNew()}>＋ {tr(lang,'newJob')}</button>
    </div>
  </header>

  {#if error}<div class="error-banner">{error} <button class="btn sm ghost" on:click={loadCore}>{tr(lang,'retry')}</button></div>{/if}
  <div class="toolbar"><div class="tabs">{#each [['all',tr(lang,'all')],['sync',tr(lang,'sync')],['commands',tr(lang,'commands')],['cron',tr(lang,'cron')],['watch',tr(lang,'watch')],['disabled',tr(lang,'disabled')]] as tab}<button class="tab" class:sel={filter===tab[0]} on:click={()=>filter=tab[0]}>{tab[1]} <span class="cnt">{counts[tab[0] as keyof typeof counts]}</span></button>{/each}</div><input class="search" bind:value={query} type="search" placeholder={tr(lang,'search')}></div>
  <div class="section-h">{tr(lang,'jobs')}</div>
  <main class="jobs">
    {#if loading}{#each [1,2,3] as _}<div class="skeleton"></div>{/each}{:else if !visible.length}<div class="empty"><div class="big">⇄</div><b>{tr(lang,'empty')}</b><p>{tr(lang,'emptyHint')}</p><button class="btn pri" on:click={()=>openNew()}>＋ {tr(lang,'newJob')}</button></div>{:else}{#each visible as job (job.id)}<JobCard {job} run={runState.byJob[job.id]} logs={runState.byJob[job.id] ? (runState.logs[runState.byJob[job.id].id] ?? []) : []} records={histories[job.id] ?? []} historyOpen={!!openHistory[job.id]} {lang} onRun={doRun} onStop={doStop} onEdit={openEdit} onClone={clone} onToggle={toggle} onDelete={remove} onHistory={toggleHistory}/>{/each}{/if}
  </main>
</div>

{#if editorOpen}<JobEditor {client} current={editorJob} initial={editorInitial} {lang} onSave={saveJob} onClose={()=>editorOpen=false}/>{/if}
{#if importOpen}<SystemImportModal {client} {lang} onImport={openNew} onClose={()=>importOpen=false}/>{/if}
{#if remotesOpen}<RemotesModal client={new ApiClient()} items={remoteList} {lang} onReload={loadRemotes} onClose={()=>remotesOpen=false}/>{/if}
{#if settingsOpen}<SettingsModal client={new ApiClient()} current={settings} {lang} onSaved={(value)=>settings=value} onClose={()=>settingsOpen=false}/>{/if}
<div class="toast" class:show={!!toastMessage} class:err={toastType==='err'} class:ok={toastType==='ok'}>{toastMessage}</div>
