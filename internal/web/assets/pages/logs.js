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

  /* ── 进场：本页所需数据（realtime：进入即刷新，见 app.js enter()） ── */
  realtime:true,
  load(){ this.loadKeys(); this.loadLogs(1); },
});
