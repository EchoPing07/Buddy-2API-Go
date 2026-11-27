/* ═══ 页面层：日志（PAGE(key, def) 注册本页专属状态与动作） ═══ */
PAGE('logs', {
    /* ── 日志 ── */
  get logAccountOptions(){
    if(this.accounts && Array.isArray(this.accounts.items) && this.accounts.items.length){
      return this.accounts.items;
    }
    return (this.pool && Array.isArray(this.pool.accounts)) ? this.pool.accounts : [];
  },
  loadLogsDebounced(){
    clearTimeout(this._logT);
    this._logT = setTimeout(()=>this.loadLogs(1), 500);
  },
  async loadLogs(page){
    if(page<1) page = 1;
    const f = this.logFilter;
    const qs = new URLSearchParams({page, page_size:this.logPageSize});
    if(f.model) qs.set('model', f.model);
    if(f.key_id) qs.set('key_id', f.key_id);
    if(f.status) qs.set('status', f.status);
    if(f.account_uid) qs.set('account_uid', f.account_uid);
    try{
      const r = await this.api('/admin/logs?'+qs);
      this.logs = r.logs||[]; this.logTotal = r.total||0; this.logPage = page;
    }catch(e){ this.toast(e.message,'err'); }
  },

  /** 账号 uid → 备注 / 昵称（日志表的「账号」列）。
      日志只存 uid，展示名优先查档案列表与号池快照；优先用户备注，其次昵称，查不到显示 uid 后 8 位。 */
  poolAccountName(uid){
    if(!uid) return '—';
    const a = (this.accounts.items||[]).find(x=>this.accountUID(x)===uid || x.uid===uid)
      || ((this.pool && this.pool.accounts)||[]).find(x=>this.accountUID(x)===uid || x.uid===uid);
    if(a && (a.remark || a.nickname)) return a.remark || a.nickname;
    return '…'+uid.slice(-8);
  },

  /* ── 进场：本页所需数据（realtime：进入即刷新，见 app.js enter()） ── */
  realtime:true,
  load(){ this.loadAccounts(); this.loadKeys(); this.loadLogs(1); },
});
