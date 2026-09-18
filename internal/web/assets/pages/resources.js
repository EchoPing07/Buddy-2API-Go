/* ═══ 页面层：余额（PAGE(key, def) 注册本页专属状态与动作） ═══ */
PAGE('resources', {
    /* ── 余额 ── */
  toggleHideDepleted(){
    this.hideDepleted = !this.hideDepleted;
    localStorage.setItem('buddy2api:hideDepleted', this.hideDepleted?'1':'0');
  },
  // 已用完 = 剩余为 0（浮点用 <= 容差），或已过期
  isDepleted(a){ return !a.capacity_remain || a.capacity_remain<=1e-9 || a.expired===true },
  get resourceAccounts(){ return this.hideDepleted ? (this.resources.accounts||[]).filter(a=>!this.isDepleted(a)) : (this.resources.accounts||[]) },
  get resourceHiddenCount(){ return (this.resources.accounts||[]).filter(a=>this.isDepleted(a)).length },
  resourceCountText(){
    const total = (this.resources.accounts||[]).length;
    if(!this.hideDepleted || !this.resourceHiddenCount) return total+' 个额度包';
    return total+' 个额度包（隐藏 ' + this.resourceHiddenCount + ' 个已用完）';
  },
  async loadResources(force){
    if(this.resources._loading) return;
    this.resources._loading = true; this.loading = true;
    try{
      const r = await this.api('/admin/resources'+(force?'?force=1':''));
      this.resources = Object.assign({loaded:true}, r.data||{});
    }catch(e){
      this.toast(e.message,'err');
      this.resources.loaded = true;
    }finally{
      this.resources._loading = false; this.loading = false;
    }
  },
  quotaBadge(a){
    if(a.warn==='expired') return 'badge-err';
    return a.warn ? 'badge-warn' : 'badge-ok';
  },
  quotaBadgeText(a){
    if(a.warn==='expired') return '已过期';
    return a.warn ? a.warn+' 内到期' : '正常';
  },
  quotaBarClass(a){
    if(a.warn==='expired') return 'err';
    return a.warn ? 'warn' : '';
  },
  quotaExpire(a){
    return a.expire_time || (a.days_left!=null ? a.days_left+' 天后' : '—');
  },

  /* ── 进场：本页所需数据 ── */
  load(){ this.loadResources(false); },
});
