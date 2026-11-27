/* ═══ 页面层：任务（PAGE(key, def) 注册本页专属状态与动作） ═══ */
PAGE('growth', {
    /* ── 本页账号快速切换（不影响全局主账号） ── */
  get growthAccountOptions(){
    if(this.accounts && Array.isArray(this.accounts.items) && this.accounts.items.length){
      return this.accounts.items;
    }
    return (this.pool && Array.isArray(this.pool.accounts)) ? this.pool.accounts : [];
  },
  growthEffectiveUID(){
    if(this.growthUID) return this.growthUID;
    const opts = this.growthAccountOptions;
    const active = opts.find(x=>x.active);
    if(active) return this.accountUID(active);
    if(this.account && (this.account.raw_uid || this.account.uid)) return this.accountUID(this.account);
    return opts.length ? this.accountUID(opts[0]) : '';
  },
  growthTargetLabel(){
    const uid = this.growthEffectiveUID();
    if(!uid) return '当前主账号';
    const a = this.growthAccountOptions.find(x=>this.accountUID(x)===uid || x.uid===uid);
    if(a) return this.accountLabel(a);
    const s = (this.checkinSummary && this.checkinSummary.items || []).find(x=>x.uid===uid);
    if(s && s.label) return s.label;
    return '…'+uid.slice(-8);
  },
  withGrowthUID(path){
    const uid = this.growthUID || '';
    if(!uid) return path;
    return path + (path.indexOf('?') >= 0 ? '&' : '?') + 'uid=' + encodeURIComponent(uid);
  },
  selectGrowthUID(uid){
    const next = uid || '';
    if(next === this.growthEffectiveUID() && this.growth.loaded) return;
    this.growthUID = next;
    this.tasksResults = [];
    this.growth = Object.assign({}, this.growth, {loaded:false});
    this.checkin = {loaded:false}; this.checkinOk = false; this.checkinErr = '';
    this.tasksLoaded = false; this.compLoaded = false;
    this.loadGrowth(false);
    this.loadCheckin();
    this.loadTasks();
    this.loadCompensation();
  },

    /* ── 签到 ── */
  checkinHint(){
    const s = this.settings || {};
    let base;
    if(!s.auto_checkin){
      base = '自动签到未开启';
    }else if(s.checkin_mode==='random'){
      base = this.checkin.random_target ? `今日 ${this.checkin.random_target} 签到` : `${s.checkin_random_start}–${s.checkin_random_end} 随机签到`;
    }else{
      base = `定时签到 ${s.checkin_cron}`;
    }
    return base + (this.checkinOk && !this.checkin.active ? ' · 活动未开启' : '');
  },
  async loadCheckinSummary(){
    try{
      const r = await this.api('/admin/checkin/summary');
      this.checkinSummary = Object.assign({items:[], checked_count:0, total_count:0, loaded:true}, r||{});
    }catch(e){
      this.checkinSummary = Object.assign({}, this.checkinSummary, {loaded:true});
    }
  },
  async claimCheckinAll(){
    if(this.checkinClaimAllBusy) return;
    this.checkinClaimAllBusy = true;
    try{
      const r = await this.api('/admin/checkin/claim_all', {method:'POST'});
      const claimed = r.claimed || 0;
      const skipped = r.skipped || 0;
      const failed = r.failed || 0;
      if(failed > 0){
        this.toast('全账号签到完成：成功 '+claimed+'，跳过 '+skipped+'，失败 '+failed, claimed > 0 ? 'ok' : 'err');
      }else{
        this.toast('全账号签到完成：新签 '+claimed+' 个'+(skipped ? ('，已签/跳过 '+skipped+' 个') : ''), 'ok');
      }
      this.loadCheckinSummary();
      this.loadCheckin();
    }catch(e){
      this.toast(e.message, 'err');
    }finally{
      this.checkinClaimAllBusy = false;
    }
  },
  async loadCheckin(){
    const gen = this._accountGen;
    const seq = ++this._checkinSeq;
    const explicitUID = this.growthUID || '';
    this.checkinPending = true;
    try{
      const r = await this.api(this.withGrowthUID('/admin/checkin/status'));
      if(gen !== this._accountGen || seq !== this._checkinSeq) return;
      if(explicitUID && explicitUID !== this.growthUID) return;
      const d = r.data && r.data.data ? r.data.data : (r.data||{});
      this.checkin = Object.assign({loaded:true, random_target:r.random_target||''}, d);
      this.checkinOk = true; this.checkinErr = '';
    }catch(e){
      if(gen !== this._accountGen || seq !== this._checkinSeq) return;
      if(explicitUID && explicitUID !== this.growthUID) return;
      this.checkin = Object.assign({}, this.checkin, {loaded:true});
      this.checkinOk = false; this.checkinErr = e.message;
    }finally{
      if(gen === this._accountGen && seq === this._checkinSeq) this.checkinPending = false;
    }
  },
  async doCheckinStep(){
    try{
      const st = await this.api(this.withGrowthUID('/admin/checkin/status'));
      const d = st.data && st.data.data ? st.data.data : (st.data||{});
      if(d.today_checked_in) return {state:'skip', detail:'今日已签到'};
      const r = await this.api(this.withGrowthUID('/admin/checkin/claim'),{method:'POST'});
      const got = (r.data && r.data.data) || {};
      if(got.credit > 0) return {state:'ok', detail:'+'+got.credit+' 积分'};
      return {state:'skip', detail:'今日已签到或暂不可领取'};
    }catch(e){ return {state:'fail', detail:e.message}; }
  },
  async claimCheckin(){
    this.busy(async()=>{
      const r = await this.api(this.withGrowthUID('/admin/checkin/claim'),{method:'POST'});
      const d = (r.data && r.data.data) || {};
      if(d.credit > 0){ this.toast('签到成功，+'+d.credit+' 积分','ok'); }
      else{ this.toast('今日已签到或暂不可领取'); }
      this.loadCheckin();
      this.loadCheckinSummary();
    })();
  },

    /* ── 任务 ── */
  async loadGrowth(force){
    const gen = this._accountGen;
    const seq = ++this._growthSeq;
    const explicitUID = this.growthUID || '';
    this.growthErr='';
    try{
      const r = await this.api(this.withGrowthUID('/admin/growth/overview'+(force?'?force=1':'')));
      if(gen !== this._accountGen || seq !== this._growthSeq) return;
      if(explicitUID && explicitUID !== this.growthUID) return;
      this.growth = Object.assign(this.growth, r, {loaded:true});
    }catch(e){
      if(gen !== this._accountGen || seq !== this._growthSeq) return;
      if(explicitUID && explicitUID !== this.growthUID) return;
      this.growthErr = e.message;
      this.toast(e.message,'err');
    }
  },
  growthRefresh(){
    clearTimeout(this.growthRunT);
    this.growthRunT = setTimeout(()=>this.loadGrowth(true), 400);
  },
  growthDegraded(k){ return (this.growth.degraded||[]).includes(k); },
  growthMakeupReady(){
    const hm=(this.growth.heatmap||{}).yesterday, mc=(this.growth.streak||{}).makeup_cards;
    return !!(hm && hm.present && hm.score===0 && mc && mc.balance>0);
  },
  growthLastRunText(){
    const lr = this.growth.last_run;
    if(!lr || !lr.at) return '';
    return this.ts(lr.at);
  },
  growthLastRunTitle(){
    const lr = this.growth.last_run;
    if(!lr || !(lr.steps||[]).length) return '';
    return (lr.steps||[]).map(s=>s.key+':'+s.status).join(' ');
  },
  growthTierSub(t){
    if(t.status==='claimed') return '已领取';
    if(t.status==='available') return '可领取';
    const days = (this.growth.streak||{}).days || 0;
    return '还差 '+Math.max(0, t.days-days)+' 天';
  },
  async _growthAction(path, body, onDone, opts){
    if(!(opts && opts.locked)){
      if(this.growthBusy) return;
      this.growthBusy = true;
    }
    try{
      const r = await this.api(this.withGrowthUID(path), {method:'POST', body:JSON.stringify(body||{})});
      if(r.ok === false){
        this.toast(r.reason==='busy' ? '已有任务在执行，请稍候' : (r.reason||r.detail||'执行失败'), 'err');
      }else if(onDone){
        onDone(r);
      }else{
        this.toast(r.detail||'已完成','ok');
      }
      this.growthRefresh();
    }catch(e){ this.toast(e.message,'err'); }
    this.growthBusy = false;
  },
  runGrowthChain(){
    const skipCheckin = this.growth.loaded && !this.growth.available;
    const steps = (skipCheckin ? '' : '签到 → ') +
      '活跃上报 → 连登自检 → 领养 → 礼包/补签 → 兑换 → 抽奖 → 旅行巡检';
    const who = this.growthTargetLabel();
    this.askConfirm('执行全部（'+who+'）',
      '将对账号「'+who+'」依次执行：'+steps+'，约需 15 秒，确认执行？',
      async ()=>{
        if(this.growthBusy) return;
        this.growthBusy = true;
        try{
          const ck = skipCheckin ? null : await this.doCheckinStep();
          if(!skipCheckin){ this.loadCheckin(); this.loadCheckinSummary(); }
          await this._growthAction('/admin/growth/run', {}, r=>{
            const n = (((r.result||{}).steps)||[]).length;
            this.toast('任务链已执行（'+n+' 步）'+(ck ? ' · 签到'+ck.detail : ''), 'ok');
            if(ck && ck.state==='fail') this.toast('签到失败：'+ck.detail, 'err');
          }, {locked:true});
        }finally{ this.growthBusy = false; }
      });
  },
  runGrowthReport(){
    const t = this.growth.today||{};
    const done = t.report_done;
    const nDisp = t.report_jitter
      ? (((+t.report_count||10)-(+t.report_jitter)) + '–' + ((+t.report_count||10)+(+t.report_jitter)))
      : String(+t.report_count||10);
    const who = this.growthTargetLabel();
    this.askConfirm('立即上报（'+who+'）',
      '将对账号「'+who+'」向上游发送 '+nDisp+' 条对话活跃上报（模拟同会话多轮，约 10 秒）。'+
      (done?'今日已完成过上报，重复上报无额外收益。':'')+'确认执行？',
      ()=> this._growthAction('/admin/growth/report', {}, r=>{
        if(r.sent === r.total) this.toast('已上报 '+r.sent+' 条','ok');
        else this.toast('上报中断：'+(r.detail||''),'err');
      }));
  },
  runGrowthAdopt(){
    this._growthAction('/admin/growth/adopt', {}, r=> this.toast(r.detail, r.status==='ok'?'ok':''));
  },
  runGrowthTravel(){
    this._growthAction('/admin/growth/travel', {}, r=>{
      const res=r.result||{};
      this.toast(res.detail||'巡检完成', res.status==='ok'?'ok':(res.status==='fail'?'err':''));
    });
  },
  runGrowthRedeem(tier){
    const t = ((this.growth.streak||{}).tiers||[]).find(x=>x.tier===tier) || {};
    const who = this.growthTargetLabel();
    this.askConfirm('兑换 '+tier+' 档奖励（'+who+'）',
      '将为账号「'+who+'」兑换「连续登录 '+tier+'」档：+'+(t.credit||0)+' 积分 +'+(t.energy||0)+' 能量 +'+(t.cards||0)+' 补签卡 +'+(t.chances||0)+' 抽奖次数，确认？',
      ()=> this._growthAction('/admin/growth/redeem', {tier}, r=>
        this.toast(r.detail, r.status==='ok'?'ok':'')));
  },
  runGrowthLottery(){
    this._growthAction('/admin/growth/lottery', {}, r=>{
      if(r.status==='ok'){
        const p = r.prize||{};
        this.toast('🎉 抽中：'+p.prize_name+(p.prize_type==='credit'?'（+'+p.credit_amount+' 积分）':'（实物，请在官方渠道填写地址）'),'ok');
      }else{
        this.toast(r.detail||'暂无抽奖次数');
      }
    });
  },
  runGrowthMakeup(){
    this._growthAction('/admin/growth/makeup', {}, r=> this.toast(r.detail, r.status==='ok'?'ok':''));
  },
  runGrowthBonus(){
    this._growthAction('/admin/growth/bonus', {}, r=> this.toast(r.detail));
  },
  growthHint(){
    const s = this.settings||{};
    if(!s.auto_growth) return '自动任务未开启';
    return '自动任务：上报 '+s.growth_report_cron+' · 旅行 '+s.growth_travel_cron+' · '+this.growthCountText();
  },

    /* ── 任务中心（成长任务列表 / 一键完成 / 补领清扫 / 补偿） ── */
  tasks: [], tasksLoaded:false, tasksOk:true, tasksErr:'',
  tasksBusy:false, tasksRunning:false, tasksResults:[],
  lastSweep:null, lastQueue:null,
  comp:{}, compLoaded:false,

  tasksPendingList(){
    return (this.tasks||[]).filter(t=>!t.claimed && !t.locked && t.auto);
  },
  sortedTasks(){
    const rank = t => t.locked?3:(t.claimed?2:(t.claimable?0:1));
    return (this.tasks||[]).slice().sort((a,b)=>rank(a)-rank(b));
  },
  taskIsDone(t){ return !!(t.target>0 && t.current>=t.target); },
  taskAt(ts){
    if(!ts) return '';
    const d = new Date(ts*1000);
    const p = n => String(n).padStart(2,'0');
    return p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes());
  },
  taskProgressText(t){
    if(t.target>0) return t.current+'/'+t.target;
    return t.claimed?'已领取':(t.accept_status||'—');
  },
  taskRewardText(t){
    const parts=[];
    if(t.credit) parts.push('+'+t.credit+'分');
    if(t.energy) parts.push('+'+t.energy+'能');
    return parts.length?parts.join(' '):'—';
  },
  taskStatusText(t){
    if(t.claimed) return '已领取';
    if(t.locked) return '未解锁';
    if(t.claimable) return '可领取';
    if(t.accept_status==='accepted') return '进行中';
    return '待开始';
  },
  taskBadgeClass(t){
    if(t.claimed) return 'badge-muted';
    if(t.locked) return 'badge-muted';
    if(t.claimable) return 'badge-ok';
    if(t.accept_status==='accepted') return 'badge-warn';
    return 'badge-muted';
  },

  async loadTasks(){
    const gen = this._accountGen;
    const seq = ++this._tasksSeq;
    const explicitUID = this.growthUID || '';
    try{
      const r = await this.api(this.withGrowthUID('/admin/tasks'));
      if(gen !== this._accountGen || seq !== this._tasksSeq) return;
      if(explicitUID && explicitUID !== this.growthUID) return;
      if(r.ok === false){ this.tasksOk=false; this.tasksErr=r.detail||r.reason||''; this.tasksLoaded=true; return; }
      this.tasks = r.tasks||[];
      this.lastSweep = r.last_sweep||null;
      this.lastQueue = r.last_queue||null;
      this.tasksOk = true; this.tasksErr='';
    }catch(e){
      if(gen !== this._accountGen || seq !== this._tasksSeq) return;
      if(explicitUID && explicitUID !== this.growthUID) return;
      this.tasksOk=false; this.tasksErr=e.message;
    }finally{
      if(gen === this._accountGen && seq === this._tasksSeq) this.tasksLoaded = true;
    }
  },
  async loadCompensation(){
    const gen = this._accountGen;
    const seq = ++this._compSeq;
    const explicitUID = this.growthUID || '';
    try{
      const r = await this.api(this.withGrowthUID('/admin/compensation'));
      if(gen !== this._accountGen || seq !== this._compSeq) return;
      if(explicitUID && explicitUID !== this.growthUID) return;
      this.comp = (r.ok===false) ? {ok:false} : r;
    }catch(e){
      if(gen !== this._accountGen || seq !== this._compSeq) return;
      if(explicitUID && explicitUID !== this.growthUID) return;
      this.comp = {ok:false};
    }finally{
      if(gen === this._accountGen && seq === this._compSeq) this.compLoaded = true;
    }
  },
  async loadAll(force){
    const gen = this._accountGen;
    const explicit = this.growthUID;
    await Promise.all([this.loadAccounts(), this.loadPool()]);
    if(gen !== this._accountGen || explicit !== this.growthUID) return;
    this.loadCheckinSummary();
    this.loadGrowth(force); this.loadCheckin(); this.loadTasks(); this.loadCompensation(); this.loadSettings();
  },

  async _taskAction(path, body){
    if(this.tasksBusy) return null;
    this.tasksBusy = true;
    try{
      const r = await this.api(this.withGrowthUID(path),{method:'POST', body:JSON.stringify(body||{})});
      if(r.ok === false){
        this.toast(r.detail || (r.reason==='busy'?'已有任务动作在执行，请稍候':(r.reason||'执行失败')),'err');
        return null;
      }
      return r;
    }catch(e){ this.toast(e.message,'err'); return null; }
    finally{ this.tasksBusy = false; this.loadTasks(); this.loadGrowth(true); }
  },

  runTaskAuto(code){
    const t = (this.tasks||[]).find(x=>x.task_code===code)||{};
    const who = this.growthTargetLabel();
    this.askConfirm('一键完成 '+code+'（'+who+'）',
      '将对账号「'+who+'」执行「'+(t.title||code)+'」的行为链，完成后自动领奖。确认执行？',
      async ()=>{
        if(this.tasksBusy) return;
        this.tasksBusy = true;
        try{
          const r = await this.api(this.withGrowthUID('/admin/tasks/auto'),{method:'POST', body:JSON.stringify({task_code:code})});
          const res = (r.result)||{};
          this.toast(res.message||r.detail||'已执行', res.status==='error'?'err':'ok');
        }catch(e){ this.toast(e.message,'err'); }
        finally{ this.tasksBusy = false; this.loadTasks(); this.loadGrowth(true); }
      });
  },
  async runTaskClaim(code){
    const r = await this._taskAction('/admin/tasks/claim',{task_code:code});
    if(r){
      if(r.already_claimed) this.toast(r.detail||'该奖励此前已领取');
      else this.toast('已领取 +'+(r.credit||0)+' 分 +'+(r.energy||0)+' 能','ok');
    }
  },
  async runTaskAccept(codes){
    const r = await this._taskAction('/admin/tasks/accept',{task_codes:codes});
    if(r) this.toast('已接受 '+(r.accepted||0)+' 个任务','ok');
  },
  runTasksAutoAll(){
    const who = this.growthTargetLabel();
    this.askConfirm('一键完成全部（'+who+'）',
      '将对账号「'+who+'」依次执行全部支持一键完成的任务（部分含真实对话，约需数分钟）。期间不能重复触发。确认？',
      async ()=>{
        if(this.tasksRunning) return;
        this.tasksRunning = true;
        try{
          const r = await this.api(this.withGrowthUID('/admin/tasks/auto_all'),{method:'POST'});
          if(r.ok === false){ this.toast(r.detail||r.reason||'执行失败','err'); return; }
          this.tasksResults = r.results||[];
          this.toast('执行完成：领奖 '+(r.claimed||0)+' 项，+'+(r.credit||0)+' 分 +'+(r.energy||0)+' 能','ok');
        }catch(e){ this.toast(e.message,'err'); }
        finally{
          this.tasksRunning = false;
          this.loadTasks(); this.loadGrowth(true); this.loadCompensation();
        }
      });
  },
  async runTasksSweep(){
    const r = await this._taskAction('/admin/tasks/sweep',{});
    if(r){
      const s = r.stats||{};
      if(s.reason) this.toast('未执行：'+(s.reason==='busy'?'已有任务动作在执行':s.reason));
      else this.toast('清扫完成：接受 '+s.accepted+' 项，领奖 '+s.claimed+' 项（+'+(s.credits||0)+' 分）','ok');
    }
  },
  async runCompensationClaim(){
    const r = await this._taskAction('/admin/compensation/claim',{});
    if(r){ this.toast(r.detail||'已处理', r.claimed?'ok':''); this.loadCompensation(); }
  },

  /* ── 进场：本页所需数据（realtime：进入即刷新，见 app.js enter()） ── */
  realtime:true,
  load(){ this.loadAll(false); },
});
