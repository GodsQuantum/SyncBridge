<script lang="ts">
  import type { ApiClient } from '../api/client';
  import { saveSettings } from '../api/syncbridge';
  import type { Settings } from '../domain/types';
  import { tr, type Lang } from '../i18n';
  export let client: ApiClient;
  export let lang: Lang = 'en';
  export let current: Settings = {notifyUrl:'',notifyAll:false};
  export let onSaved: (settings: Settings) => void = () => {};
  export let onClose: () => void = () => {};
  let notifyUrl=current.notifyUrl, notifyAll=current.notifyAll, busy=false, message='', failed=false;
  async function save(test=false){busy=true;message='';try{const result=await saveSettings(client,{notifyUrl:notifyUrl.trim(),notifyAll,test});failed=!!result.testError;message=test?(result.testError||tr(lang,'testSent')):tr(lang,'settingsSaved');onSaved({notifyUrl:notifyUrl.trim(),notifyAll});}catch(e){failed=true;message=e instanceof Error?e.message:String(e)}finally{busy=false}}
</script>
<div class="scrim open" role="presentation" on:click|self={onClose}><section class="modal" role="dialog" aria-modal="true" style="max-width:620px">
<header class="modal-h"><h2>{tr(lang,'notifications')}</h2><span class="flex"></span><button class="btn ghost sm icon-btn" on:click={onClose}>✕</button></header>
<div class="modal-b">{#if message}<div class:info-banner={!failed} class:error-banner={failed}>{message}</div>{/if}<div class="field"><div class="field-label">{tr(lang,'notificationUrl')}</div><input bind:value={notifyUrl} type="text" placeholder="https://ntfy.sh/my-topic"><div class="hint">{tr(lang,'notificationHint')}</div></div><label class="switch-row"><input bind:checked={notifyAll} type="checkbox"><span class="switch-txt"><b>{tr(lang,'notifySuccess')}</b><span>{tr(lang,'failuresOnly')}</span></span></label></div>
<footer class="modal-f"><button class="btn ghost" disabled={busy} on:click={()=>save(true)}>{tr(lang,'test')}</button><button class="btn pri" disabled={busy} on:click={()=>save(false)}>{tr(lang,'save')}</button></footer>
</section></div>
