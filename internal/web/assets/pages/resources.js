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
    // 在途期间到来的 force 不能静默丢：强刷只发生在换账号后，而在途那次拿的正是**上一账号**的
    // 余额，丢了就把旧数据留在页面上。标志放壳层：返回时 this.resources 被整体替换，放里面会丢。
    if(this.resources._loading){
      if(force) this._resourceRefetch = true;
      return;
    }
    const gen = this._accountGen;   // 捕获代次：换账号/删凭证后本响应作废
    const rs = this.resources;      // 本轮挂 _loading 的对象（换代后它已被丢弃）
    this.resources._loading = true; this.loading = true;
    try{
      const r = await this.api('/admin/resources'+(force?'?force=1':''));
      if(gen !== this._accountGen) return;   // 期间已换代：旧账号的余额不得写回
      this.resources = Object.assign({loaded:true}, r.data||{});
    }catch(e){
      if(gen !== this._accountGen) return;   // 旧请求的失败也不该再报错
      this.toast(e.message,'err');
      this.resources.loaded = true;
    }finally{
      // 只清自己那份（rs）：换代后新对象可能已有自己的在途请求，碰它会误清对方的去重标志
      rs._loading = false;
      this.loading = false;
      // 待重取只在未换代时补发；换代后 refreshAccountData 已拉过一遍，再补发是白发
      if(gen === this._accountGen && this._resourceRefetch){
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
  load(){ this.loadResources(false); },
});
