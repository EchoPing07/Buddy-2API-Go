/* ═══ 页面层：账号（PAGE(key, def) 注册本页专属状态与动作） ═══ */
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
        this.toast('已清除凭证','ok');
        this.loadAccount(); this.loadHealth();
      })();
    });
  },
  async exportAccount(){
    this.busy(async()=>{
      const r = await fetch('/admin/account/export');
      if(r.status===401){ this.loggedIn=false; throw new Error('登录已过期'); }
      if(!r.ok){ const d = await r.json().catch(()=>({})); throw new Error(d.error||('HTTP '+r.status)); }
      const blob = await r.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url; a.download = 'token.json';
      document.body.appendChild(a); a.click(); a.remove();
      URL.revokeObjectURL(url);
      this.toast('凭证已导出','ok');
    })();
  },
  onImportFile(ev){
    const file = ev.target.files && ev.target.files[0];
    if(!file) return;
    this.importForm.filename = file.name;
    const reader = new FileReader();
    reader.onload = ()=>{
      try{
        const obj = JSON.parse(reader.result);
        this.importForm.access_token = obj.access_token || '';
        this.importForm.refresh_token = obj.refresh_token || '';
        this.importForm.domain = obj.domain || '';
        this.toast('已读取文件，确认后导入','ok');
      }catch(e){ this.toast('文件不是合法 JSON: '+e.message,'err'); }
    };
    reader.onerror = ()=>this.toast('读取文件失败','err');
    reader.readAsText(file);
    ev.target.value = '';
  },
  async importAccount(){
    const f = this.importForm;
    if(!f.access_token || !f.access_token.trim()){ this.toast('access_token 不能为空','err'); return; }
    this.busy(async()=>{
      const r = await this.api('/admin/account/import',{method:'POST',body:JSON.stringify({
        access_token:f.access_token.trim(),
        refresh_token:f.refresh_token.trim(),
        domain:f.domain.trim(),
      })});
      this.toast('导入成功：'+(r.nickname||r.uid||'已保存'),'ok');
      this.importModal = false;
      this.importForm = {access_token:'',refresh_token:'',domain:'',filename:''};
      this.loadAccount(); this.loadHealth();
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
        this.loadAccount(); this.loadHealth(); this.loadModels();
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
  load(){ this.loadAccount(); },
});
