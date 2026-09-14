<script lang="ts">
  import { onMount } from 'svelte';
  import { tr, type Lang } from '$lib/i18n';

  let configured=false, envManaged=false, ready=false, user='', password='', password2='', busy=false, message='', error=false;
  let lang: Lang = 'en';

  onMount(async()=>{
    lang=(localStorage.getItem('sb_lang') as Lang)||'en';
    document.documentElement.lang=lang;
    try{const r=await fetch('/api/auth/status');const s=await r.json();if(s.authed){location.replace('/');return}configured=!!s.configured;envManaged=!!s.envManaged}catch{}finally{ready=true}
  });
  function toggleLang(){lang=lang==='en'?'fr':'en';localStorage.setItem('sb_lang',lang);document.documentElement.lang=lang}
  async function submit(){
    busy=true;message='';error=false;
    try{
      if(!configured){if(user.trim().length<3)throw new Error(tr(lang,'authUserShort'));if(password.length<6)throw new Error(tr(lang,'authPassShort'));if(password!==password2)throw new Error(tr(lang,'authMismatch'))}
      const r=await fetch(configured?'/api/auth/login':'/api/auth/register',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({User:user.trim(),Password:password})});
      if(!r.ok)throw new Error(await r.text());
      location.replace('/');
    }catch(e){error=true;message=e instanceof Error?e.message:String(e);busy=false}
  }
</script>
<svelte:head><title>SyncBridge — {configured?tr(lang,'authTitle'):tr(lang,'authCreate')}</title></svelte:head>
<div class="auth-shell"><div class="auth-card">
  <div class="auth-top"><div class="brand"><span class="mark"><i></i><i></i><i></i></span> SyncBridge</div><button class="btn ghost sm" type="button" on:click={toggleLang}>{lang==='en'?'FR':'EN'}</button></div>
  <div class="sub">cron · watch · rsync · scripts</div>
  {#if ready}
    <h1>{configured?tr(lang,'authTitle'):tr(lang,'authCreate')}</h1>
    <p class="lead">{configured?tr(lang,'authLead'):tr(lang,'authCreateLead')}</p>
    <form on:submit|preventDefault={submit}>
      <label for="auth-user">{tr(lang,'authUsername')}</label><input id="auth-user" bind:value={user} type="text" autocomplete="username">
      <label for="auth-pass">{tr(lang,'authPassword')}</label><input id="auth-pass" bind:value={password} type="password" autocomplete={configured?'current-password':'new-password'}>
      {#if !configured}<label for="auth-pass2">{tr(lang,'authConfirm')}</label><input id="auth-pass2" bind:value={password2} type="password" autocomplete="new-password">{/if}
      <button class="btn pri auth-submit" disabled={busy}>{busy?'…':configured?tr(lang,'authSignIn'):tr(lang,'authCreateButton')}</button>
    </form>
    {#if message}<div class="msg" class:err={error}>{message}</div>{/if}
    {#if envManaged}<div class="note">{tr(lang,'authManaged')}</div>{/if}
  {:else}<div class="hint">{tr(lang,'loading')}</div>{/if}
</div></div>
<style>
  .auth-shell{min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px}.auth-card{width:100%;max-width:390px;background:var(--surface);border:1px solid var(--line);border-radius:14px;box-shadow:0 24px 80px rgba(0,0,0,.5);padding:30px 28px}.auth-top{display:flex;align-items:center;justify-content:space-between;gap:14px}.auth-card .brand{font-size:20px;margin-bottom:4px}.sub{color:var(--dim);font-family:var(--mono);font-size:11.5px;margin-bottom:24px}.auth-card h1{font-size:17px;margin-bottom:5px}.lead{color:var(--muted);font-size:12.5px;margin-bottom:20px;line-height:1.5}.auth-card label{display:block;font-size:11px;color:var(--muted);margin:0 0 6px;text-transform:uppercase;letter-spacing:.05em;font-family:var(--mono);font-weight:600}.auth-card input{width:100%;background:var(--bg);border:1px solid var(--line);color:var(--ink);border-radius:8px;padding:11px 13px;font-family:var(--mono);font-size:13px;margin-bottom:15px}.auth-card input:focus{outline:none;border-color:var(--cyan-d);box-shadow:0 0 0 3px var(--cyan-glow)}.auth-submit{width:100%;justify-content:center;margin-top:4px}.msg{font-size:12px;margin-top:14px;padding:9px 11px;border:1px solid #2b5c42;border-radius:7px;color:var(--green);background:#0f1c16}.msg.err{color:var(--red);border-color:#6b2f38;background:#1f1214}.note{font-size:11px;color:var(--dim);margin-top:16px;line-height:1.5}
</style>
