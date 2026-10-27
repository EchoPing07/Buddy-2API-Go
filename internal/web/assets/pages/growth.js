/* ═══ 页面层：任务（PAGE(key, def) 注册本页专属状态与动作） ═══ */
PAGE('growth', {
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
    // 活动未开启时手动领取必然失败，提前在提示里说明，避免用户点了才报错
    return base + (this.checkinOk && !this.checkin.active ? ' · 活动未开启' : '');
  },
  async loadCheckin(){
    const gen = this._accountGen;   // 捕获代次：换账号/删凭证后本响应作废
    this.checkinPending = true;
    try{
      const r = await this.api('/admin/checkin/status');
      if(gen !== this._accountGen) return;   // 期间已换代：旧的签到状态不得写回
      const d = r.data && r.data.data ? r.data.data : (r.data||{});
      this.checkin = Object.assign({loaded:true, random_target:r.random_target||''}, d);
      this.checkinOk = true; this.checkinErr = '';
    }catch(e){
      if(gen !== this._accountGen) return;   // 旧请求的失败不该再降级/报错
      // 国际版/未登录/上游异常：卡片自身降级为空态 + 重试，不额外 toast 刷屏。
      // 标记 loaded=true 以区分「加载中」与「已失败」，否则首帧/请求在途会假报失败。
      this.checkin = Object.assign({}, this.checkin, {loaded:true});
      this.checkinOk = false; this.checkinErr = e.message;
    }finally{
      // 只在未换代时清：换代后新请求会自己置位/清位，此处不应误清新请求的 pending
      if(gen === this._accountGen) this.checkinPending = false;
    }
  },
  // 执行链内的签到一步：已签则跳过，失败只记结果不阻断整链
  // 返回 state: ok=领到积分 | skip=已签到/不可领（正常态）| fail=请求失败（需提示）
  async doCheckinStep(){
    try{
      const st = await this.api('/admin/checkin/status');
      const d = st.data && st.data.data ? st.data.data : (st.data||{});
      if(d.today_checked_in) return {state:'skip', detail:'今日已签到'};
      const r = await this.api('/admin/checkin/claim',{method:'POST'});
      const got = (r.data && r.data.data) || {};
      if(got.credit > 0) return {state:'ok', detail:'+'+got.credit+' 积分'};
      return {state:'skip', detail:'今日已签到或暂不可领取'};
    }catch(e){ return {state:'fail', detail:e.message}; }
  },
  async claimCheckin(){
    this.busy(async()=>{
      const r = await this.api('/admin/checkin/claim',{method:'POST'});
      const d = (r.data && r.data.data) || {};
      if(d.credit > 0){ this.toast('签到成功，+'+d.credit+' 积分','ok'); }
      else{ this.toast('今日已签到或暂不可领取'); }
      this.loadCheckin();
    })();
  },

    /* ── 任务 ── */
  async loadGrowth(force){
    const gen = this._accountGen;   // 捕获代次：换账号/删凭证后本响应作废
    this.growthErr='';
    try{
      const r = await this.api('/admin/growth/overview'+(force?'?force=1':''));
      if(gen !== this._accountGen) return;   // 期间已换代：旧账号的总览不得写回
      this.growth = Object.assign(this.growth, r, {loaded:true});
    }catch(e){
      if(gen !== this._accountGen) return;   // 旧请求的失败不该再弹错/写空态
      // 整体空态 + 重试；不置 loaded，避免卡片以空数据渲染误导
      this.growthErr = e.message;
      this.toast(e.message,'err');
    }
  },
  growthRefresh(){
    clearTimeout(this.growthRunT);
    this.growthRunT = setTimeout(()=>this.loadGrowth(true), 400);
  },
  growthDegraded(k){ return (this.growth.degraded||[]).includes(k); },
  // 补签条件：昨日有格且漏签 && 补签卡余额 > 0
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
    // locked：按本档所需天数与当前连登天数作差，不要拿 remaining_days（它只描述下一档）
    const days = (this.growth.streak||{}).days || 0;
    return '还差 '+Math.max(0, t.days-days)+' 天';
  },
  // 通用动作执行器：path + body + 自定义成功回调（缺省 toast detail）
  // opts.locked=true：调用方已持有 growthBusy（组合动作，如「执行全部」先签到再跑链），
  // 这里不再重复判断/加锁（否则会被开头的 busy 判断直接 return，后半段永远不执行）
  async _growthAction(path, body, onDone, opts){
    if(!(opts && opts.locked)){
      if(this.growthBusy) return;
      this.growthBusy = true;
    }
    try{
      const r = await this.api(path, {method:'POST', body:JSON.stringify(body||{})});
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
    // 签到是国内版功能：overview 明确不可用（global / 未登录）时跳过，别发注定失败的上游调用
    const skipCheckin = this.growth.loaded && !this.growth.available;
    const steps = (skipCheckin ? '' : '签到 → ') +
      '活跃上报 → 连登自检 → 领养 → 礼包/补签 → 兑换 → 抽奖 → 旅行巡检';
    this.askConfirm('执行全部',
      '将依次执行：'+steps+'，约需 15 秒，确认执行？',
      async ()=>{
        if(this.growthBusy) return;
        this.growthBusy = true;   // 贯穿签到 + 任务链两段，防中途重复点击
        try{
          const ck = skipCheckin ? null : await this.doCheckinStep();
          if(!skipCheckin) this.loadCheckin();
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
    // 缺省 count 交由服务端按配置 + 波动数随机，前端文案按区间展示
    const nDisp = t.report_jitter
      ? (((+t.report_count||10)-(+t.report_jitter)) + '–' + ((+t.report_count||10)+(+t.report_jitter)))
      : String(+t.report_count||10);
    this.askConfirm('立即上报',
      '将向上游发送 '+nDisp+' 条对话活跃上报（模拟同会话多轮，约 10 秒）。'+
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
    this.askConfirm('兑换 '+tier+' 档奖励',
      '兑换「连续登录 '+tier+'」档：+'+(t.credit||0)+' 积分 +'+(t.energy||0)+' 能量 +'+(t.cards||0)+' 补签卡 +'+(t.chances||0)+' 抽奖次数，确认？',
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

  // 待完成：未领、未锁、有自动化动作
  tasksPendingList(){
    return (this.tasks||[]).filter(t=>!t.claimed && !t.locked && t.auto);
  },
  // 排序：可领取 → 待完成 → 已领 → 锁定（同组内保持服务端顺序）
  sortedTasks(){
    const rank = t => t.locked?3:(t.claimed?2:(t.claimable?0:1));
    return (this.tasks||[]).slice().sort((a,b)=>rank(a)-rank(b));
  },
  taskIsDone(t){ return !!(t.target>0 && t.current>=t.target); },
  // 落库时间戳 → 本地时间串（sweep/queue 结果里带 at 秒级 unix）
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
    try{
      const r = await this.api('/admin/tasks');
      if(r.ok === false){ this.tasksOk=false; this.tasksErr=r.detail||r.reason||''; this.tasksLoaded=true; return; }
      this.tasks = r.tasks||[];
      this.lastSweep = r.last_sweep||null;
      this.lastQueue = r.last_queue||null;
      this.tasksOk = true; this.tasksErr='';
    }catch(e){
      this.tasksOk=false; this.tasksErr=e.message;
    }finally{ this.tasksLoaded = true; }
  },
  async loadCompensation(){
    try{
      const r = await this.api('/admin/compensation');
      this.comp = (r.ok===false) ? {ok:false} : r;
    }catch(e){ this.comp = {ok:false}; }
    finally{ this.compLoaded = true; }
  },
  loadAll(force){
    this.loadGrowth(force); this.loadCheckin(); this.loadTasks(); this.loadCompensation(); this.loadSettings();
  },

  async _taskAction(path, body){
    if(this.tasksBusy) return null;
    this.tasksBusy = true;
    try{
      const r = await this.api(path,{method:'POST', body:JSON.stringify(body||{})});
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
    this.askConfirm('一键完成 '+code,
      '将执行「'+(t.title||code)+'」的行为链，完成后自动领奖。确认执行？',
      async ()=>{
        if(this.tasksBusy) return;
        this.tasksBusy = true;
        try{
          const r = await this.api('/admin/tasks/auto',{method:'POST', body:JSON.stringify({task_code:code})});
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
    this.askConfirm('一键完成全部',
      '将依次执行全部支持一键完成的任务（部分含真实对话，约需数分钟）。期间不能重复触发。确认？',
      async ()=>{
        if(this.tasksRunning) return;
        this.tasksRunning = true;
        try{
          const r = await this.api('/admin/tasks/auto_all',{method:'POST'});
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
