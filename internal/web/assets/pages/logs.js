/* ═══ 页面层：日志（PAGE(key, def) 注册本页专属状态与动作） ═══ */
PAGE('logs', {
    /* ── 日志 ── */
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
    try{
      const r = await this.api('/admin/logs?'+qs);
      this.logs = r.logs||[]; this.logTotal = r.total||0; this.logPage = page;
    }catch(e){ this.toast(e.message,'err'); }
  },

  /** 号池账号 uid → 昵称（日志表的「账号」列）。
      日志只存 uid（写日志时账号名可能还没拉到），昵称用账号页已拉到的档案列表翻译；
      查不到就显示 uid 后 8 位而不是空白——空白会被误读成"没走号池"。 */
  poolAccountName(uid){
    if(!uid) return '—';
    const a = (this.accounts.items||[]).find(x=>x.uid===uid);
    return (a && a.nickname) ? a.nickname : ('…'+uid.slice(-8));
  },

  /* ── 进场：本页所需数据（realtime：进入即刷新，见 app.js enter()） ── */
  realtime:true,
  load(){ this.loadKeys(); this.loadLogs(1); },
});
