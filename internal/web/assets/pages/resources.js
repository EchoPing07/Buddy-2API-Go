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
  get resourceAccountOptions(){
    if(this.accounts && Array.isArray(this.accounts.items) && this.accounts.items.length){
      return this.accounts.items;
    }
    if(this.resources && Array.isArray(this.resources.accounts_summary) && this.resources.accounts_summary.length){
      return this.resources.accounts_summary;
    }
    return (this.pool && Array.isArray(this.pool.accounts)) ? this.pool.accounts : [];
  },
  resourceCountText(){
    const total = (this.resources.accounts||[]).length;
    if(!this.hideDepleted || !this.resourceHiddenCount) return total+' 个额度包';
    return total+' 个额度包（隐藏 ' + this.resourceHiddenCount + ' 个已用完）';
  },
  selectResourceUID(uid){
    const next = uid || 'all';
    this.resourceUID = next;
    this._resourceRefetch = false;
    this.resources._loading = false;
    this.resources.loaded = false;
    this.loadResources(false);
  },
  async loadResources(force){
    // 在途期间到来的 force 不能静默丢：强刷只发生在换账号后，而在途那次拿的正是**上一账号**的
    // 余额，丢了就把旧数据留在页面上。标志放壳层：返回时 this.resources 被整体替换，放里面会丢。
    if(this.resources._loading){
      if(force) this._resourceRefetch = true;
      return;
    }
    const uid = this.resourceUID || 'all';
    const gen = this._accountGen;   // 捕获代次：换账号/删凭证后本响应作废
    const seq = ++this._resourceSeq;
    const rs = this.resources;      // 本轮挂 _loading 的对象（换代后它已被丢弃）
    this.resources._loading = true; this.loading = true;
    try{
      const qs = new URLSearchParams();
      qs.set('uid', uid);
      if(force) qs.set('force', '1');
      const r = await this.api('/admin/resources?'+qs.toString());
      if(gen !== this._accountGen || seq !== this._resourceSeq) return;
      if(r && r.mode === 'all'){
        this.resources = Object.assign({loaded:true, mode:'all', accounts:[], accounts_summary:[]}, r);
      }else{
        const data = (r && r.data) ? r.data : (r || {});
        this.resources = Object.assign({
          loaded:true,
          mode:'single',
          updated_at: r && r.updated_at,
          uid: (r && r.uid) || uid,
          label: (r && r.label) || '',
          accounts:[],
        }, data);
      }
    }catch(e){
      if(gen !== this._accountGen || seq !== this._resourceSeq) return;
      this.toast(e.message,'err');
      this.resources.loaded = true;
    }finally{
      rs._loading = false;
      if(seq === this._resourceSeq) this.loading = false;
      if(gen === this._accountGen && seq === this._resourceSeq && this._resourceRefetch){
        this._resourceRefetch = false; this.loadResources(true);
      }
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
  load(){ this.loadAccounts(); this.loadResources(false); },
});
