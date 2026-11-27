/* ═══ 页面层：账号（PAGE(key, def) 注册本页专属状态与动作） ═══ */

/* JWT payload 解码（不验签）：只用于弹窗里的「将要导入谁」预览。
   真正的 uid / nickname / domain 归一在服务端（auth.EnrichFromJWT），这里解不出来就留空。 */
function jwtClaims(tok){
  try{
    const p = String(tok||'').split('.');
    if(p.length < 2) return {};
    const b64 = p[1].replace(/-/g,'+').replace(/_/g,'/');
    const pad = b64 + '==='.slice((b64.length+3) % 4);
    return JSON.parse(atob(pad)) || {};
  }catch(e){ return {}; }
}

PAGE('account', {
    /* ── 账号 ── */
  async loadAccount(){
    try{ this.account = await this.api('/admin/account'); }catch(e){ this.toast(e.message,'err'); }
  },
  /** 刷新确认文案：区分「主动刷新」「无 refresh_token」两种场景 */
  refreshTokenHint(){
    if(!this.account.has_refresh) return '当前凭证未保存 refresh_token，刷新会直接失败，请改用「导入凭证」补填或重新登录。';
    return this.account.expired
      ? '凭证已过期，刷新后会用 refresh_token 换取新的 access_token。'
      : '当前凭证仍在有效期内，刷新会立刻作废现有 access_token 并换取新的一对凭证。';
  },
  refreshToken(){
    this.askConfirm('刷新 Token',
      '确定要立即刷新 OAuth 凭证吗？',
      ()=> this.busy(async()=>{
        const r = await this.api('/admin/account/refresh',{method:'POST'});
        this.toast('已刷新，新有效期 '+r.expires_human,'ok');
        this.loadAccount(); this.loadHealth(); this.loadPool();
      })(),
      {detail:this.refreshTokenHint(), okText:'刷新', danger:false});
  },
  async testAccount(){
    this.busy(async()=>{
      const r = await this.api('/admin/account/test',{method:'POST'});
      r.ok ? this.toast('凭证可用，上游连通正常','ok') : this.toast('测试失败：'+r.message,'err');
    })();
  },
  deleteAccount(){
    this.askConfirm('退出登录','将删除本地保存的主账号 OAuth 凭证，确定继续？', ()=>{
      this.busy(async()=>{
        await this.api('/admin/account',{method:'DELETE'});
        // 凭证已删：就地清空账号口径状态并撤销保活（此刻不能再强刷，无凭证只会报错）
        this.resetAccountData();
        this.toast('已清除凭证','ok');
        this.loadAccount(); this.loadHealth(); this.loadAccounts(); this.loadPool();
      })();
    });
  },

    /* ── 导出凭证（弹窗：单个 / 批量） ── */
  /** 打开导出弹窗：默认「单个导出」（用户最常导的就是当前这份）；无活动账号时退到批量 */
  openExport(){
    this.openExportWith(this.account.logged_in ? 'current' : 'selected');
  },
  openExportWith(scope){
    this.exportForm = {scope, uids: [], touched: false};
    if(scope === 'selected') this.exportForm.uids = this.accounts.items.map(a=>a.uid);
    this.exportModal = true;
  },
  setExportScope(scope){
    this.exportForm.scope = scope;
    if(scope === 'selected' && !this.exportForm.touched){
      this.exportForm.uids = this.accounts.items.map(a=>a.uid);
    }
  },
  toggleExportUID(uid){
    this.exportForm.touched = true;
    const u = this.exportForm.uids;
    const i = u.indexOf(uid);
    if(i >= 0) u.splice(i,1); else u.push(uid);
  },
  exportAllSelected(){
    const n = this.accounts.items.length;
    return n > 0 && this.exportForm.uids.length === n;
  },
  exportSelectAll(){
    this.exportForm.touched = true;
    this.exportForm.uids = this.exportAllSelected() ? [] : this.accounts.items.map(a=>a.uid);
  },
  exportCount(){
    return this.exportForm.scope === 'current'
      ? (this.account.logged_in ? 1 : 0)
      : this.exportForm.uids.length;
  },
  async exportAccounts(){
    const s = this.exportForm.scope;
    if(this.exportCount() === 0) return;
    this.busy(async()=>{
      let url = '/admin/account/export?scope=' + encodeURIComponent(s);
      if(s === 'selected') url += '&uids=' + encodeURIComponent(this.exportForm.uids.join(','));
      const r = await fetch(url);
      if(r.status === 401){ this.loggedIn = false; throw new Error('登录已过期'); }
      if(!r.ok){ const d = await r.json().catch(()=>({})); throw new Error(d.error || ('HTTP '+r.status)); }
      const blob = await r.blob();
      const cd = (r.headers && r.headers.get) ? (r.headers.get('Content-Disposition')||'') : '';
      const m = /filename="([^"]+)"/.exec(cd);
      const count = Number((r.headers && r.headers.get) ? r.headers.get('X-Export-Count') : 0) || this.exportCount();
      this.download(blob, m ? m[1] : 'buddy2api-accounts.json');
      const missing = (r.headers && r.headers.get) ? r.headers.get('X-Export-Missing') : '';
      this.exportModal = false;
      this.toast('已导出 ' + count + ' 个账号' + (missing ? '（部分所选账号不存在，已跳过）' : ''),'ok');
    })();
  },
  download(blob, filename){
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url; a.download = filename;
    document.body.appendChild(a); a.click(); a.remove();
    URL.revokeObjectURL(url);
  },

    /* ── 导入凭证 ── */
  openImport(){
    this.importMode = 'single';
    this.batchForm = {filename:'',items:[],error:'',bundle:null};
    this.batchResult = null;
    this.importModal = true;
  },
  setImportMode(mode){
    this.importMode = mode;
    this.batchForm.error = '';
  },
  onImportFile(ev){
    const file = ev.target.files && ev.target.files[0];
    if(!file) return;
    const reader = new FileReader();
    reader.onload = ()=>{
      let obj;
      try{ obj = JSON.parse(reader.result); }
      catch(e){ this.importForm.filename = file.name; this.toast('文件不是合法 JSON: '+e.message,'err'); return; }
      if(obj && obj.format){
        this.setImportMode('batch');
        this.parseBatchBundle(file.name, reader.result);
        return;
      }
      this.importForm.filename = file.name;
      this.importForm.access_token = obj.access_token || '';
      this.importForm.refresh_token = obj.refresh_token || '';
      this.importForm.domain = obj.domain || '';
      this.importForm.remark = obj.remark || '';
      this.toast('已读取文件，确认后导入','ok');
    };
    reader.onerror = ()=>this.toast('读取文件失败','err');
    reader.readAsText(file);
    ev.target.value = '';
  },
  async importAccount(activate){
    const f = this.importForm;
    if(!f.access_token || !f.access_token.trim()){ this.toast('access_token 不能为空','err'); return; }
    this.importActivate = !!activate;
    this.busy(async()=>{
      const r = await this.api('/admin/account/import',{method:'POST',body:JSON.stringify({
        access_token:f.access_token.trim(),
        refresh_token:(f.refresh_token||'').trim(),
        domain:(f.domain||'').trim(),
        remark:(f.remark||'').trim(),
        activate:!!activate,
      })});
      this.importModal = false;
      this.importForm = {access_token:'',refresh_token:'',domain:'',remark:'',filename:''};
      this.loadAccounts();
      if(activate){
        this.toast('已导入并启用：'+(r.remark||r.nickname||r.uid||'已保存'),'ok');
        this.refreshAccountData();
      }else{
        const who = r.remark||r.nickname||r.uid||'已存档';
        this.toast('已导入：'+who+(this.account.logged_in
          ? '，已加入多账号调度'
          : '（当前无主账号，请在列表中点「设为主账号」）'),'ok');
        this.loadPool();
      }
    })();
  },

    /* ── 批量导入（凭证包 → 仅入档，恒不切换主账号） ── */
  onBatchFile(ev){
    const file = ev.target.files && ev.target.files[0];
    ev.target.value = '';
    if(!file) return;
    const reader = new FileReader();
    reader.onload = ()=>this.parseBatchBundle(file.name, reader.result);
    reader.onerror = ()=>{ this.batchForm = {filename:file.name, items:[], error:'读取文件失败', bundle:null}; };
    reader.readAsText(file);
  },
  parseBatchBundle(filename, text){
    this.batchResult = null;
    this.batchForm = {filename, items:[], error:'', bundle:null};
    let obj;
    try{ obj = JSON.parse(text); }
    catch(e){ this.batchForm.error = '文件不是合法 JSON: ' + e.message; return; }
    if(!obj || typeof obj !== 'object'){
      this.batchForm.error = '文件内容不是凭证包';
      return;
    }
    if(obj.format !== 'buddy2api.accounts'){
      if(obj.access_token){
        this.setImportMode('single');
        this.importForm = {
          access_token: obj.access_token || '',
          refresh_token: obj.refresh_token || '',
          domain: obj.domain || '',
          remark: obj.remark || '',
          filename,
        };
        this.toast('这是单个账号凭证，已切到「单个账号」导入','ok');
        return;
      }
      this.batchForm.error = '这不是本项目导出的凭证包（缺少 format 标识）';
      return;
    }
    const list = Array.isArray(obj.accounts) ? obj.accounts : [];
    if(!list.length){ this.batchForm.error = '凭证包里没有账号'; return; }
    this.batchForm.items = list.map(t=>{
      const c = jwtClaims(t.access_token);
      return {
        uid: t.uid || c.sub || '',
        nickname: t.nickname || c.nickname || '',
        remark: t.remark || '',
        domain: t.domain || '',
      };
    });
    this.batchForm.bundle = obj;
  },
  async importBatch(){
    const f = this.batchForm;
    if(!f.bundle || !f.items.length){ this.toast('请先选择凭证包','err'); return; }
    this.busy(async()=>{
      f.error = '';
      this.batchResult = null;
      let r, failBody = null;
      try{
        r = await this.api('/admin/account/import/batch',{method:'POST',body:JSON.stringify(f.bundle)});
      }catch(e){
        failBody = e.body || null;
        if(failBody && Array.isArray(failBody.items) && failBody.items.length){
          this.batchResult = {imported:failBody.imported||0, failed:failBody.failed||0,
                               total:failBody.total||0, overwrote:failBody.overwrote||0,
                               items:failBody.items};
          this.loadAccounts();
          this.toast('导入失败：' + (failBody.failed||failBody.items.length) + ' 个账号未导入（见下方明细）','err');
          return;
        }
        f.error = e.message;
        throw e;
      }
      this.batchResult = {imported:r.imported||0, failed:r.failed||0, total:r.total||0,
                          overwrote:r.overwrote||0, items:r.items||[]};
      this.loadAccounts();
      f.items = [];
      f.filename = '';
      f.bundle = null;
      const ow = r.overwrote ? ('，'+r.overwrote+' 个覆盖了原有档案') : '';
      if(r.failed){
        this.toast('已导入 '+r.imported+' 个'+ow+'，'+r.failed+' 个失败（见下方明细）', r.imported ? 'ok' : 'err');
      }else{
        this.toast('已导入 '+r.imported+' 个账号'+ow+'，已加入多账号调度','ok');
      }
      this.loadPool();
    })();
  },

    /* ── 多账号档案 ── */
  async loadAccounts(){
    try{
      const r = await this.api('/admin/accounts');
      if(r && Array.isArray(r.items)) this.accounts = {items:r.items, loaded:true};
    }catch(e){ /* 忽略 */ }
  },
  /** 行内更新账号备注：仅就地更新目标行与活动/号池副本，不整表重拉，失败回滚输入框。 */
  async updateAccountRemark(target, rawRemark, el){
    if(!target) return;
    const items = (this.accounts && Array.isArray(this.accounts.items)) ? this.accounts.items : [];
    const targetID = target.id || target.uid;
    if(!targetID) return;
    const findIdx = ()=> items.findIndex(x=>x.uid===targetID || (x.raw_uid && x.raw_uid===targetID) || (target.raw_uid && (x.uid===target.raw_uid || x.raw_uid===target.raw_uid)) || (target===this.account && x.active));
    const idx = findIdx();
    const isActive = target === this.account
      || (idx >= 0 && !!items[idx].active)
      || !!(this.account && this.account.logged_in && (this.account.id===targetID || this.account.uid===targetID));
    const prevRemark = (idx >= 0 ? items[idx].remark : (isActive ? this.account.remark : target.remark)) || '';
    const nextRemark = String(rawRemark == null ? '' : rawRemark).trim();
    if(nextRemark === prevRemark){
      if(el) el.value = prevRemark;
      return;
    }
    const reqUID = idx >= 0 ? items[idx].uid : targetID;
    const before = idx >= 0 ? Object.assign({}, items[idx]) : null;
    try{
      const r = await this.api('/admin/accounts/'+encodeURIComponent(reqUID),{
        method:'PUT',
        body:JSON.stringify({remark:nextRemark}),
      });
      const saved = (r && typeof r.remark === 'string') ? r.remark : nextRemark;
      const j = findIdx();
      if(j >= 0){
        const row = (r && r.item) ? r.item : Object.assign({}, before, {remark:saved});
        items.splice(j, 1, row);
      }
      if(isActive && this.account){
        this.account = Object.assign({}, this.account, {remark:saved});
      }
      if(this.pool && Array.isArray(this.pool.accounts)){
        const rawUID = (r && r.raw_uid) || (idx >= 0 && items[idx].raw_uid) || (isActive && this.account && (this.account.raw_uid || this.account.uid)) || target.raw_uid || target.uid;
        const pIdx = this.pool.accounts.findIndex(x=>x.uid===rawUID || x.uid===reqUID);
        if(pIdx >= 0){
          this.pool.accounts.splice(pIdx, 1, Object.assign({}, this.pool.accounts[pIdx], {remark:saved}));
        }
      }
      if(el) el.value = saved;
      this.toast(saved ? '备注已保存' : '备注已清除','ok');
    }catch(e){
      const j = findIdx();
      if(j >= 0 && before) items.splice(j, 1, Object.assign({}, before));
      if(el) el.value = prevRemark;
      this.toast(e.message,'err');
    }
  },
  switchAccount(a){
    this.askConfirm('设为主账号',
      '将「'+(a.remark||a.nickname||a.uid.slice(-8))+'」设为主账号？',
      ()=>this.busy(async()=>{
        const r = await this.api('/admin/accounts/'+encodeURIComponent(a.uid)+'/activate',{method:'POST'});
        const targetRaw = (r && r.uid) || this.accountUID(a);
        if(this.accounts && Array.isArray(this.accounts.items)){
          this.accounts.items.forEach(x=>{ x.active = (this.accountUID(x)===targetRaw || x.uid===a.uid); });
        }
        if(this.pool && Array.isArray(this.pool.accounts)){
          this.pool.accounts.forEach(x=>{ x.active = (x.uid===targetRaw); });
        }
        this.toast('已设为主账号：'+(r.remark||r.nickname||r.uid||''),'ok');
        this.refreshAccountData();
        this.loadAccounts();
      })(), {okText:'设为主账号', danger:false});
  },
  removeAccount(a){
    this.askConfirm('移除账号',
      '从列表删除「'+(a.remark||a.nickname||a.uid.slice(-8))+'」的本地存档？当前主账号不受影响。',
      ()=>this.busy(async()=>{
        await this.api('/admin/accounts/'+encodeURIComponent(a.uid),{method:'DELETE'});
        this.toast('已移除','ok');
        this.loadAccounts();
        this.loadPool();
      })(), {okText:'移除'});
  },

    /* ═══ 统一多账号调度控制台 ═══ */
  get poolStrategies(){
    return [
      {key:'expire_first',  label:'临期优先'},
      {key:'balanced',      label:'均衡分摊'},
      {key:'credits_first', label:'高余额优先'},
    ];
  },
  get poolStrategy(){
    return (this.pool && this.pool.strategy) || (this.settings && this.settings.pool_strategy) || 'expire_first';
  },
  get unifiedAccounts(){
    if(this.pool && Array.isArray(this.pool.accounts)) return this.pool.accounts;
    return (this.accounts && Array.isArray(this.accounts.items)) ? this.accounts.items : [];
  },
  refreshPoolConsole(){
    this.loadPool();
    this.loadAccounts();
  },
  setPoolStrategy(strategy){
    if(!['expire_first','balanced','credits_first'].includes(strategy)) return;
    if(this.pool && this.pool.strategy === strategy) return;
    this.busy(async()=>{
      const r = await this.api('/admin/pool/strategy', {method:'PUT', body:JSON.stringify({strategy})});
      const next = (r && r.strategy) || strategy;
      if(this.pool) this.pool.strategy = next;
      if(this.settings) this.settings.pool_strategy = next;
      this.toast('路由策略已切换为「'+this.poolStrategyLabel(next)+'」','ok');
      this.loadPool();
    })();
  },
  /** 调度状态键：三色语义（绿可用 / 黄部分限额 / 红整号不可用） */
  poolStateKey(a){
    if(a.manual_disabled || a.auto_disabled) return 'down';
    if(a.partial_blocked || (a.model_cooldowns && a.model_cooldowns.length)) return 'partial';
    return 'ok';
  },
  poolStateText(a){
    if(a.manual_disabled) return '手动禁用';
    if(a.auto_disabled === 'quota') return '额度耗尽';
    if(a.auto_disabled === 'credential') return '凭证失效';
    if(a.auto_disabled) return a.auto_disabled;
    if(a.partial_blocked || (a.model_cooldowns && a.model_cooldowns.length)) return '部分限额';
    return '可用';
  },
  poolStateClass(a){
    const k = this.poolStateKey(a);
    return k === 'ok' ? 'badge-ok' : (k === 'partial' ? 'badge-warn' : 'badge-err');
  },
  coolLeft(until){
    const s = Math.max(0, until - Math.floor(Date.now()/1000));
    if(s <= 0) return '已到期';
    const h = Math.floor(s/3600), m = Math.floor((s%3600)/60);
    return h > 0 ? (h+'h '+m+'m') : (m+'m');
  },
  poolDisable(a){
    this.askConfirm('禁用账号',
      '在多账号调度中禁用「'+(a.remark||a.nickname||a.uid.slice(-8))+'」？',
      ()=>this.busy(async()=>{
        await this.api('/admin/pool/accounts/'+encodeURIComponent(a.uid)+'/disable',{method:'POST'});
        this.toast('已禁用','ok');
        this.loadPool(); this.loadAccounts();
      })(),
      {detail:'手动禁用后定时探测不会自动恢复这个账号，需要手动点「启用」。', okText:'禁用'});
  },
  poolEnable(a){
    this.busy(async()=>{
      await this.api('/admin/pool/accounts/'+encodeURIComponent(a.uid)+'/enable',{method:'POST'});
      this.toast('已启用','ok');
      this.loadPool(); this.loadAccounts();
    })();
  },
  poolProbe(a){
    this.busy(async()=>{
      const r = await this.api('/admin/pool/accounts/'+encodeURIComponent(a.uid)+'/probe',{method:'POST'});
      if(r.skipped) this.toast('该账号已手动禁用，探测不会恢复它','err');
      else if(r.ok) this.toast('探测成功：额度 '+r.credits,'ok');
      else this.toast('探测未恢复：'+(r.msg||r.reason||''),'err');
      this.loadPool(); this.loadAccounts();
    })();
  },
  poolClearCool(a, c){
    this.busy(async()=>{
      await this.api('/admin/pool/cool/'+encodeURIComponent(a.uid)+'/'+encodeURIComponent(c.model)+'/clear',{method:'POST'});
      this.toast('已清除 '+c.model+' 的冷却','ok');
      this.loadPool();
    })();
  },
  poolResetSessions(){
    this.askConfirm('重置会话绑定',
      '清空全部会话粘性绑定？下一次请求会按当前路由策略重新分配账号。',
      ()=>this.busy(async()=>{
        const r = await this.api('/admin/pool/reset',{method:'POST'});
        this.toast('已清空 '+r.cleared+' 条绑定','ok');
        this.loadPool();
      })(), {okText:'重置', danger:false});
  },
  poolProbeAll(){
    this.busy(async()=>{
      const r = await this.api('/admin/pool/probe_all',{method:'POST'});
      const ok = (r.items||[]).filter(x=>x.ok).length;
      const skipped = (r.items||[]).filter(x=>x.skipped).length;
      this.toast('探测完成：'+ok+' 个可用'+(skipped?('，'+skipped+' 个手动禁用跳过'):''),'ok');
      this.loadPool(); this.loadAccounts();
    })();
  },

    /* ── OAuth 设备流 ── */
  async oauthStart(){
    this.busy(async()=>{
      const r = await this.api('/admin/account/oauth/start',{method:'POST'});
      this.oauth.state = r.state;
      this.oauth.auth_url = r.auth_url;
      this.oauth.poll_msg = '等待授权中…';
      this.oauth._deadline = Date.now() + 5*60*1000;
      window.open(r.auth_url, '_blank');
      this.oauth._timer = setInterval(()=>this.oauthPoll(), 3000);
      this.oauthPoll();
    })();
  },
  async oauthPoll(){
    if(!this.oauth.state) return;
    if(Date.now() > this.oauth._deadline){ this.oauthStop('超时未完成授权'); return; }
    try{
      const r = await this.api('/admin/account/oauth/poll?state='+encodeURIComponent(this.oauth.state));
      if(r.status==='success'){
        this.oauthStop();
        this.toast('登录成功：'+(r.account.remark||r.account.nickname||r.account.uid),'ok');
        this.refreshAccountData(); this.loadModels(); this.loadAccounts();
      }else if(r.status==='error'){
        this.oauthStop(r.message);
      }else{
        this.oauth.poll_msg = '等待授权中（'+new Date().toLocaleTimeString('zh-CN',{hour12:false})+'）';
      }
    }catch(e){ /* 网络抖动忽略，下轮继续 */ }
  },
  oauthStop(errMsg){
    if(this.oauth._timer){ clearInterval(this.oauth._timer); this.oauth._timer = null; }
    if(errMsg){ this.toast(errMsg,'err'); this.oauth.poll_msg = errMsg; }
    setTimeout(()=>{ this.oauth.state=''; this.oauth.auth_url=''; }, errMsg?2500:400);
  },
  oauthCancel(){ this.oauthStop(); },

  /* ── 进场：本页所需数据 ── */
  load(){ this.loadAccount(); this.loadAccounts(); this.loadPool(); },
});
