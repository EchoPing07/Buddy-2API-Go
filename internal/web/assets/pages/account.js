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
        this.loadAccount(); this.loadHealth();
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
    this.askConfirm('退出登录','将删除本地保存的 OAuth 凭证，确定继续？', ()=>{
      this.busy(async()=>{
        await this.api('/admin/account',{method:'DELETE'});
        // 凭证已删：就地清空账号口径状态并撤销保活（此刻不能再强刷，无凭证只会报错）
        this.resetAccountData();
        this.toast('已清除凭证','ok');
        this.loadAccount(); this.loadHealth(); this.loadAccounts();
      })();
    });
  },

    /* ── 导出凭证（弹窗：单个 / 批量） ── */
  /** 打开导出弹窗：默认「单个导出」（用户最常导的就是当前这份）；无活动账号时退到批量 */
  openExport(){
    this.openExportWith(this.account.logged_in ? 'current' : 'selected');
  },
  openExportWith(scope){
    // uids 恒为数组（模板里 x-show 隐藏的区块仍会被 Alpine 求值，null 会直接报错）；
    // 「是否已被用户改过」另用 exportForm.touched 记，用于区分下面两种空选：
    // 首次进入批量的默认全选 vs 用户主动「取消全选」。
    this.exportForm = {scope, uids: [], touched: false};
    // 批量导出默认全选：要全部只需点导出，要部分则取消勾选（比逐个勾更省事）
    if(scope === 'selected') this.exportForm.uids = this.accounts.items.map(a=>a.uid);
    this.exportModal = true;
  },
  /** 切方式。
   * 「进入批量时自动全选」只在用户尚未动过选择时做：
   * 否则用户点过「取消全选」清空选择、再切到单个又切回批量时，其清空意图会被静默还原成全选，
   * 结果一点导出就把所有账号导出去了。 */
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
  /** 是否已全选（全选按钮的文案与「已全选」提示都看它） */
  exportAllSelected(){
    const n = this.accounts.items.length;
    return n > 0 && this.exportForm.uids.length === n;
  },
  exportSelectAll(){
    this.exportForm.touched = true;
    this.exportForm.uids = this.exportAllSelected() ? [] : this.accounts.items.map(a=>a.uid);
  },
  /** 本次将导出多少个账号（0 → 主按钮置灰） */
  exportCount(){
    return this.exportForm.scope === 'current'
      ? (this.account.logged_in ? 1 : 0)
      : this.exportForm.uids.length;
  },
  /** 导出凭证包并下载。范围与选中项由后端统一组装成 auth.Bundle（单账号同构）。 */
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
      // 文件名以服务端 Content-Disposition 为准（含 uid / 时间戳），拿不到再退到通用名
      const cd = (r.headers && r.headers.get) ? (r.headers.get('Content-Disposition')||'') : '';
      const m = /filename="([^"]+)"/.exec(cd);
      const count = Number((r.headers && r.headers.get) ? r.headers.get('X-Export-Count') : 0) || this.exportCount();
      this.download(blob, m ? m[1] : 'buddy2api-accounts.json');
      const missing = (r.headers && r.headers.get) ? r.headers.get('X-Export-Missing') : '';
      this.exportModal = false;
      this.toast('已导出 ' + count + ' 个账号' + (missing ? '（部分所选账号不存在，已跳过）' : ''),'ok');
    })();
  },
  /** 触发浏览器下载。blob 需要即时 revoke，故 a.click() 后立刻回收 URL。 */
  download(blob, filename){
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url; a.download = filename;
    document.body.appendChild(a); a.click(); a.remove();
    URL.revokeObjectURL(url);
  },

    /* ── 导入凭证 ── */
  /** 打开导入弹窗：默认单个账号导入，批量状态每次重开都清空（避免上一轮的残留结果） */
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
      // 选错了凭证包：直接切到批量模式并沿用这个文件（比报一个「请切换到批量」的 toast 少一步操作）
      if(obj && obj.format){
        this.setImportMode('batch');
        this.parseBatchBundle(file.name, reader.result);
        return;
      }
      this.importForm.filename = file.name;
      this.importForm.access_token = obj.access_token || '';
      this.importForm.refresh_token = obj.refresh_token || '';
      this.importForm.domain = obj.domain || '';
      this.toast('已读取文件，确认后导入','ok');
    };
    reader.onerror = ()=>this.toast('读取文件失败','err');
    reader.readAsText(file);
    ev.target.value = '';
  },
  async importAccount(activate){
    const f = this.importForm;
    if(!f.access_token || !f.access_token.trim()){ this.toast('access_token 不能为空','err'); return; }
    // 两个按钮共用全局 loading（会同时置灰），但只有被点的那一个转圈
    this.importActivate = !!activate;
    this.busy(async()=>{
      const r = await this.api('/admin/account/import',{method:'POST',body:JSON.stringify({
        access_token:f.access_token.trim(),
        refresh_token:f.refresh_token.trim(),
        domain:f.domain.trim(),
        activate:!!activate,
      })});
      this.importModal = false;
      this.importForm = {access_token:'',refresh_token:'',domain:'',filename:''};
      // 账号列表必须先刷：仅导入时它是用户能看到的唯一反馈
      this.loadAccounts();
      if(activate){
        this.toast('已导入并启用：'+(r.nickname||r.uid||'已保存'),'ok');
        // 换账号：余额/签到/任务重拉（后端已清缓存，但前端 keep-alive，旧数据会残留）
        this.refreshAccountData();
      }else{
        const who = r.nickname||r.uid||'已存档';
        // 无活动账号时「仅导入」不会让应用能用，提示去列表启用（不默默替用户切账号）
        this.toast('已导入：'+who+(this.account.logged_in
          ? '，可在「账号切换」中启用'
          : '（当前无活动账号，请在下方列表点「切换」启用）'),'ok');
      }
      // 号池开启时，新导入的账号要立刻参与分发（后端 ReloadMembers 只在下一次开关变更时跑）
      this.loadPool();
    })();
  },

    /* ── 批量导入（凭证包 → 仅入档，恒不切换当前账号） ── */
  /** 选凭证包：本地先解析并预览，格式不对就地报错，不必先发一轮请求 */
  onBatchFile(ev){
    const file = ev.target.files && ev.target.files[0];
    ev.target.value = '';   // 清空以便重复选同一个文件
    if(!file) return;
    const reader = new FileReader();
    reader.onload = ()=>this.parseBatchBundle(file.name, reader.result);
    reader.onerror = ()=>{ this.batchForm = {filename:file.name, items:[], error:'读取文件失败', bundle:null}; };
    reader.readAsText(file);
  },
  /** 解析凭证包并填预览。单账号模式误选凭证包时也走这里（见 onImportFile）。 */
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
      // 选错了（单体 token.json）：切到单个账号模式并回填，比报一个「请切到单个」少一步操作
      if(obj.access_token){
        this.setImportMode('single');
        this.importForm = {
          access_token: obj.access_token || '',
          refresh_token: obj.refresh_token || '',
          domain: obj.domain || '',
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
    // 预览用的 uid / 昵称：包里往往只存了 access_token，从 JWT 现解出来给用户看
    this.batchForm.items = list.map(t=>{
      const c = jwtClaims(t.access_token);
      return {
        uid: t.uid || c.sub || '',
        nickname: t.nickname || c.nickname || '',
        domain: t.domain || '',
      };
    });
    this.batchForm.bundle = obj;   // 原样回传服务端：客户端不做归一，避免与后端两套口径
  },
  /** 批量导入。语义是「仅导入」：只入档，不切换活动账号（见后端 accountImportBatch）。 */
  async importBatch(){
    const f = this.batchForm;
    if(!f.bundle || !f.items.length){ this.toast('请先选择凭证包','err'); return; }
    this.busy(async()=>{
      // 先清上一轮的错误与结果：整包失败后重试成功时，旧的红色错误会与「导入完成」同时挂在弹窗里
      f.error = '';
      this.batchResult = null;
      let r, failBody = null;
      try{
        r = await this.api('/admin/account/import/batch',{method:'POST',body:JSON.stringify(f.bundle)});
      }catch(e){
        failBody = e.body || null;
        // 全部失败：后端回 400 + 逐条明细（imported:0），这是最需要看明细的情形。
        // 若无差别当错误抛出，用户只能看到「HTTP 400」，真正原因（逐条 JWT 非法）全被丢掉。
        if(failBody && Array.isArray(failBody.items) && failBody.items.length){
          this.batchResult = {imported:failBody.imported||0, failed:failBody.failed||0,
                               total:failBody.total||0, overwrote:failBody.overwrote||0,
                               items:failBody.items};
          this.loadAccounts();   // 「全部失败」通常意味着一个都没写进去，但刷新无害且能对齐真实状态
          this.toast('导入失败：' + (failBody.failed||failBody.items.length) + ' 个账号未导入（见下方明细）','err');
          return;
        }
        // 其余（格式 / 版本 / 空账号 / 超限 / 网络）：把服务端原文留在弹窗里，别只弹一个飘走的 toast
        f.error = e.message;
        throw e;
      }
      this.batchResult = {imported:r.imported||0, failed:r.failed||0, total:r.total||0,
                          overwrote:r.overwrote||0, items:r.items||[]};
      // 账号列表是批量导入唯一的产出，必须刷新；当前账号未变，故不碰账号口径数据
      this.loadAccounts();
      f.items = [];
      f.filename = '';
      f.bundle = null;
      const ow = r.overwrote ? ('，'+r.overwrote+' 个覆盖了原有档案') : '';
      if(r.failed){
        this.toast('已导入 '+r.imported+' 个'+ow+'，'+r.failed+' 个失败（见下方明细）', r.imported ? 'ok' : 'err');
      }else{
        this.toast('已导入 '+r.imported+' 个账号'+ow+'，可在「账号切换」中启用','ok');
      }
      this.loadPool();
    })();
  },

    /* ── 多账号档案 ── */
  /** 拉取档案列表；失败静默（账号页其余内容不依赖它） */
  async loadAccounts(){
    try{
      const r = await this.api('/admin/accounts');
      if(r && Array.isArray(r.items)) this.accounts = {items:r.items, loaded:true};
    }catch(e){ /* 忽略 */ }
  },
  switchAccount(a){
    this.askConfirm('切换账号',
      '切换到「'+(a.nickname||a.uid.slice(-8))+'」？',
      ()=>this.busy(async()=>{
        const r = await this.api('/admin/accounts/'+encodeURIComponent(a.uid)+'/activate',{method:'POST'});
        this.toast('已切换到 '+(r.nickname||r.uid||''),'ok');
        // 后端已清账号口径缓存，前端 keep-alive，必须原地强刷
        this.refreshAccountData();
        this.loadAccounts();
      })());
  },
  removeAccount(a){
    this.askConfirm('移除账号',
      '从列表删除「'+(a.nickname||a.uid.slice(-8))+'」的本地存档？当前登录不受影响。',
      ()=>this.busy(async()=>{
        await this.api('/admin/accounts/'+encodeURIComponent(a.uid),{method:'DELETE'});
        this.toast('已移除','ok');
        this.loadAccounts();
        this.loadPool();   // 后端已一并清理该号的池状态
      })(), {okText:'移除'});
  },

    /* ═══ 号池状态 ═══
       只在开关开启时渲染；关闭时整块不显示，也不发 /admin/pool 请求。 */
  async loadPool(){
    if(!this.labs.pool_enabled){ this.pool = null; return; }
    try{ this.pool = await this.api('/admin/pool'); }catch(e){ this.pool = null; }
  },
  /** 号池账号行的状态键：三色语义（绿可用 / 黄部分限额 / 红整号不可用） */
  poolStateKey(a){
    if(a.manual_disabled || a.auto_disabled) return 'down';
    if(a.model_cooldowns && a.model_cooldowns.length) return 'partial';
    return 'ok';
  },
  poolStateText(a){
    if(a.manual_disabled) return '手动禁用';
    if(a.auto_disabled === 'quota') return '额度用尽';
    if(a.auto_disabled === 'credential') return '凭证失效';
    if(a.auto_disabled) return a.auto_disabled;
    if(a.model_cooldowns && a.model_cooldowns.length) return '部分模型限额';
    return '可用';
  },
  poolStateClass(a){
    const k = this.poolStateKey(a);
    return k === 'ok' ? 'badge-ok' : (k === 'partial' ? 'badge-warn' : 'badge-err');
  },
  /** 冷却剩余时间文案（前端算，避免后端为每个模型返回一个「还剩多久」的易变字段） */
  coolLeft(until){
    const s = Math.max(0, until - Math.floor(Date.now()/1000));
    if(s <= 0) return '已到期';
    const h = Math.floor(s/3600), m = Math.floor((s%3600)/60);
    return h > 0 ? (h+'h '+m+'m') : (m+'m');
  },
  poolDisable(a){
    this.askConfirm('禁用账号',
      '在号池中禁用「'+(a.nickname||a.uid.slice(-8))+'」？',
      ()=>this.busy(async()=>{
        await this.api('/admin/pool/accounts/'+encodeURIComponent(a.uid)+'/disable',{method:'POST'});
        this.toast('已禁用','ok');
        this.loadPool(); this.loadAccounts();
      })(),
      {detail:'手动禁用后探测**不会**自动恢复这个账号，需要手动点「启用」。', okText:'禁用'});
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
      '清空全部会话粘性绑定？下一次请求会重新分配账号。',
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
        this.toast('登录成功：'+(r.account.nickname||r.account.uid),'ok');
        // 换账号，同导入；loadModels 单独留着：模型列表跟 region 走不跟账号走
        this.refreshAccountData(); this.loadModels(); this.loadAccounts(); this.loadPool();
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
