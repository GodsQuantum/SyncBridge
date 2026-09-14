<script lang="ts">
  import type { Job, RunRecord, RunSnapshot } from '../domain/types';
  import { cronHuman, formatDuration } from '../domain/format';
  import { rsyncProgress } from '../domain/runs';
  import { tr, type Lang } from '../i18n';

  export let job: Job;
  export let run: RunSnapshot | undefined = undefined;
  export let logs: string[] = [];
  export let records: RunRecord[] = [];
  export let historyOpen = false;
  export let lang: Lang = 'en';
  export let onRun: (job: Job, dry: boolean) => void = () => {};
  export let onStop: (run: RunSnapshot) => void = () => {};
  export let onEdit: (job: Job) => void = () => {};
  export let onClone: (job: Job) => void = () => {};
  export let onToggle: (job: Job) => void = () => {};
  export let onDelete: (job: Job) => void = () => {};
  export let onHistory: (job: Job) => void = () => {};

  $: active = !!run && !['succeeded','failed','killed','timed_out','start_failed','skipped_overlap'].includes(run.status);
  $: terminalError = !!run && ['failed','timed_out','start_failed'].includes(run.status);
  $: lastLog = logs.length ? logs[logs.length - 1] : '';
  $: progress = rsyncProgress(lastLog);
  $: mode = job.action.type === 'sync' ? (job.action.sync?.mode ?? '') : '';
  $: flowSource = job.action.type === 'sync' ? job.action.sync?.source : undefined;
  $: flowDest = job.action.type === 'sync' ? job.action.sync?.dest : undefined;
  $: statusClass = active ? 'running' : terminalError ? 'error' : run?.status === 'succeeded' ? 'ok' : '';
  $: statusText = active ? (run?.status === 'queued' ? tr(lang,'queued') : tr(lang,'running')) : (run?.status ?? 'idle');
</script>

<article class:disabled={!job.enabled} class="job">
  <div class="job-row">
    <span class="dot {statusClass}" aria-hidden="true"></span>
    <div class="job-body">
      <div class="status-line">
        <div class="job-title">{job.name}</div>
        {#if !job.enabled}<span class="off-lbl">{tr(lang,'disabled')}</span>{/if}
        {#if job.needsReview}<span class="tag review">{tr(lang,'review')}</span>{/if}
        {#if run}<span class="status-copy">{statusText}</span>{/if}
      </div>
      {#if job.action.type === 'sync'}
        <div class="flow"><span class="p" title={flowSource}>{flowSource}</span><span class="ar">→</span><span class="p" title={flowDest}>{flowDest}</span></div>
      {:else if job.action.type === 'command'}
        <div class="flow"><span class="p" title={job.action.command}>{job.action.command}</span></div>
      {:else}
        <div class="flow"><span class="p" title={job.action.scriptPath}>{job.action.scriptPath}</span></div>
      {/if}
      <div class="tags">
        <span class="tag eng">{job.action.type === 'sync' ? job.action.sync?.engine : job.action.type}</span>
        {#if mode}<span class="tag {mode}">{mode}</span>{/if}
        <span class="tag trig">{job.trigger === 'cron' ? cronHuman(job.cron ?? '', lang) : job.trigger}</span>
        {#if job.scheduler.owner === 'system'}<span class="tag sys">system</span>{/if}
        <span class="tag">rev {job.revision}</span>
      </div>
    </div>
    <div class="job-act">
      {#if active && run}
        <button class="btn sm kill" on:click={() => onStop(run)}>{tr(lang,'stop')}</button>
      {:else}
        <button class="btn sm pri" disabled={!job.enabled || job.needsReview} on:click={() => onRun(job, false)}>{tr(lang,'run')}</button>
        {#if job.action.type === 'sync'}<button class="btn sm" disabled={!job.enabled || job.needsReview} on:click={() => onRun(job, true)}>{tr(lang,'dry')}</button>{/if}
      {/if}
      <button class="btn sm ghost" on:click={() => onEdit(job)}>{tr(lang,'edit')}</button>
    </div>
  </div>

  <div class="job-meta">
    <span>ID {job.id}</span>
    <span>{job.execution.overlap ?? 'skip'}</span>
    {#if run?.startedAt && active}<span>{formatDuration((Date.now() - new Date(run.startedAt).getTime()) / 1000)}</span>{/if}
    {#if run?.finishedAt}<span>{new Date(run.finishedAt).toLocaleString()}</span>{/if}
    {#if run?.error}<span class="danger">{run.error}</span>{/if}
  </div>

  {#if active}
    <div class="live-bar active">
      <div class="live-head">
        <span class="pct" class:scan={progress.scanning}>{progress.scanning ? 'scanning…' : progress.percent !== null ? `${progress.percent}%` : statusText}</span>
        {#if progress.speed}<span class="chip">{progress.speed}</span>{/if}
        {#if progress.eta}<span class="eta">ETA {progress.eta}</span>{/if}
        {#if run?.dryRun}<span class="tag violet">dry-run</span>{/if}
      </div>
      <div class="progress-track"><div class:indeterminate={progress.percent === null} class="progress-fill" style:width={progress.percent === null ? '30%' : `${progress.percent}%`}></div></div>
    </div>
  {/if}
  {#if logs.length}<pre class="console">{logs.join('\n')}</pre>{/if}

  <div class="job-meta">
    <button class="btn ghost sm" on:click={() => onHistory(job)}>{tr(lang,'history')}</button>
    <button class="btn ghost sm" on:click={() => onClone(job)}>{tr(lang,'clone')}</button>
    <button class="btn ghost sm" class:on={!job.enabled} on:click={() => onToggle(job)}>{job.enabled ? tr(lang,'disable') : tr(lang,'enable')}</button>
    <button class="btn ghost sm dgr" on:click={() => onDelete(job)}>{tr(lang,'delete')}</button>
  </div>
  {#if historyOpen}
    <div class="hist">
      {#if !records.length}<div class="hist-row">{tr(lang,'noHistory')}</div>{/if}
      {#each records as record}
        <div class="hist-row"><span class="hd {record.status === 'succeeded' ? 'ok' : 'error'}">{record.status}</span><span>{new Date(record.ts).toLocaleString()}</span><span class="hdur">{formatDuration(record.dur)}</span>{#if record.note}<span class="hnote">{record.note}</span>{/if}</div>
      {/each}
    </div>
  {/if}
</article>
