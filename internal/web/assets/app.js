/* ═══ Buddy 2API Go 管理后台前端：共享层 ═══
 * 由 shell.html 以 <script defer> 引入；页面专属动作见 assets/pages/<key>.js（PAGE 注册）。
 * 当前页 key 由服务端写入 <body data-page="…">，见 internal/web/web.go。
 * 状态字段集中在共享层（同名成员由页面层覆盖），跨页数据加载器亦放这里。
 */

/* ═══ Lucide 图标（inline SVG path） ═══ */
const ICONS = {
  dashboard:'<rect width="7" height="9" x="3" y="3" rx="1"/><rect width="7" height="5" x="14" y="3" rx="1"/><rect width="7" height="9" x="14" y="12" rx="1"/><rect width="7" height="5" x="3" y="16" rx="1"/>',
  account:'<circle cx="12" cy="8" r="5"/><path d="M20 21a8 8 0 0 0-16 0"/>',
  keys:'<circle cx="7.5" cy="15.5" r="5.5"/><path d="m21 2-9.6 9.6"/><path d="m15.5 7.5 3 3L22 7l-3-3"/>',
  wallet:'<path d="M21 12V7H5a2 2 0 0 1 0-4h14v4"/><path d="M3 5v14a2 2 0 0 0 2 2h16v-5"/><path d="M18 12a2 2 0 0 0 0 4h4v-4Z"/>',
  checkin:'<path d="M8 2v4"/><path d="M16 2v4"/><rect width="18" height="18" x="3" y="4" rx="2"/><path d="M3 10h18"/><path d="m9 16 2 2 4-4"/>',
  logs:'<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/><path d="M16 13H8"/><path d="M16 17H8"/><path d="M10 9H8"/>',
  settings:'<path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/>',
  refresh:'<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>',
  external:'<path d="M15 3h6v6"/><path d="M10 14 21 3"/><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/>',
  copy:'<rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/>',
  plus:'<path d="M5 12h14"/><path d="M12 5v14"/>',
  logout:'<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" x2="9" y1="12" y2="12"/>',
  sun:'<circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>',
  moon:'<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/>',
  monitor:'<rect width="20" height="14" x="2" y="3" rx="2"/><line x1="8" x2="16" y1="21" y2="21"/><line x1="12" x2="12" y1="17" y2="21"/>',
  menu:'<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M9 3v18"/>',
  check:'<path d="M20 6 9 17l-5-5"/>',
  chevronL:'<path d="m15 18-6-6 6-6"/>',
  chevronR:'<path d="m9 18 6-6-6-6"/>',
  activity:'<path d="M22 12h-2.48a2 2 0 0 0-1.93 1.46l-2.35 8.36a.25.25 0 0 1-.48 0L9.24 2.18a.25.25 0 0 0-.48 0l-2.35 8.36A2 2 0 0 1 4.49 12H2"/>',
  type:'<polyline points="4 7 4 4 20 4 20 7"/><line x1="9" x2="15" y1="20" y2="20"/><line x1="12" x2="12" y1="4" y2="20"/>',
  alert:'<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
  dollar:'<circle cx="12" cy="12" r="10"/><path d="M16 8h-6a2 2 0 1 0 0 4h4a2 2 0 1 1 0 4H8"/><path d="M12 6v2m0 8v2"/>',
  flame:'<path d="M8.5 14.5A2.5 2.5 0 0 0 11 12c0-1.38-.5-2-1-3-1.072-2.143-.224-4.054 2-6 .5 2.5 2 4.9 4 6.5 2 1.6 3 3.5 3 5.5a7 7 0 1 1-14 0c0-1.153.433-2.294 1-3a2.5 2.5 0 0 0 2.5 2.5z"/>',
  growth:'<path d="m3 17 2 2 4-4"/><path d="m3 7 2 2 4-4"/><path d="M13 6h8"/><path d="M13 12h8"/><path d="M13 18h8"/>',
  download:'<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="7 10 12 15 17 10"/><line x1="12" x2="12" y1="15" y2="3"/>',
  upload:'<path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><polyline points="17 8 12 3 7 8"/><line x1="12" x2="12" y1="3" y2="15"/>',
  shield:'<path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1 1 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/><path d="m9 12 2 2 4-4"/>',
  eye:'<path d="M2.06 12.35a1 1 0 0 1 0-.7 10.75 10.75 0 0 1 19.88 0 1 1 0 0 1 0 .7 10.75 10.75 0 0 1-19.88 0"/><circle cx="12" cy="12" r="3"/>',
  eyeOff:'<path d="M10.73 5.08A10.43 10.43 0 0 1 12 5c7 0 10 7 10 7a13.16 13.16 0 0 1-1.67 2.68"/><path d="M6.61 6.61A13.5 13.5 0 0 0 2 12s3 7 10 7a9.75 9.75 0 0 0 5.39-1.61"/><path d="M14.12 14.12a3 3 0 1 1-4.24-4.24"/><path d="m2 2 20 20"/>',
  box:'<path d="M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z"/><path d="m3.3 7 8.7 5 8.7-5"/><path d="M12 22V12"/>',
};

/* ═══ 页面 key 推导：'/keys' | '/keys/' | '/' → 'keys' | 'dashboard' ═══
 * 全站唯一的 key 归一规则，Go 侧（内部路由）与 JS 侧（navClick / popstate）共用同一约定：
 * 页面 key 就是路径去掉前导斜杠，根路径对应清单首项 dashboard。
 */
function pageKey(p){
  const s = String(p||'').replace(/^\/+|\/+$/g, '');
  return s === '' ? 'dashboard' : s;
}

/* ═══ 页面层注册表：各页脚本调用 PAGE(id, def) 注册自己的成员 ═══ */
const PAGES = {};
function PAGE(id, def){ PAGES[id] = def; }

/* ═══ 共享层：状态 / 主题 / 图标 / 请求工具 / 登录 ═══ */
function appShell(){
  return {
    /* ── 状态（view 由各页 data-page 在总装时注入；entered/titles 同处注入） ── */
    ready:false, loggedIn:false, loading:false, password:'',
    mobileNav:false, toasts:[], _toastSeq:0,
    theme: localStorage.getItem('buddy2api:theme') || 'system',
    health:{}, stats:{}, account:{}, keys:[],
    logs:[], logTotal:0, logPage:1, logPageSize:20,
    logFilter:{model:'',key_id:'',status:''}, _logT:0,
    keyForm:{name:'',custom_key:''}, keyModal:false,
    resources:{accounts:[],loaded:false},
    hideDepleted: localStorage.getItem('buddy2api:hideDepleted')==='1', // 默认 false = 全部显示
    checkin:{loaded:false}, checkinOk:false, checkinErr:'', checkinPending:false,
    /* ── 任务 ── */
    growth:{available:true, reason:'', loaded:false, streak:null, heatmap:null,
            buddy:null, travel:null, lottery:null, today:null, last_run:null,
            degraded:[], updated_at:0},
    growthErr:'',               // overview 请求失败信息（非空时视图整体空态 + 重试）
    growthCenterUrl:'https://www.workbuddy.cn/profile/growth-center',  // 官网个人中心（成长中心）
    growthBusy:false,           // 上报/领养等长动作锁（与全局 loading 分开：上报要 10s）
    growthRunT:0,               // 总览防抖 timer
    settings:{}, pwForm:{old_password:'',new_password:''},
    models:{models:[],ids:[],state:null,loading:false},
    importModal:false, importForm:{access_token:'',refresh_token:'',domain:'',filename:''},
    confirmBox:{open:false,title:'',message:'',detail:'',okText:'确定',danger:true,onOk:null},
    oauth:{state:'',auth_url:'',poll_msg:'等待授权中…',_timer:null,_deadline:0},
    _chartGeom:null,


    /* ── 主题 ── */
    get themeLabel(){ return this.theme==='light'?'浅色':this.theme==='dark'?'深色':'系统' },
    applyTheme(){
      const dark = this.theme==='dark' || (this.theme==='system' && matchMedia('(prefers-color-scheme: dark)').matches);
      document.documentElement.classList.toggle('dark', dark);
    },
    cycleTheme(){
      this.theme = this.theme==='light'?'dark':this.theme==='dark'?'system':'light';
      localStorage.setItem('buddy2api:theme', this.theme);
      this.applyTheme();
    },

    /* ── 图标 ── */
    ic(name){
      const p = ICONS[name];
      return p ? `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round">${p}</svg>` : '';
    },

    /* ── 初始化 ── */
    async init(){
      this.applyTheme();
      matchMedia('(prefers-color-scheme: dark)').addEventListener('change', ()=>{ if(this.theme==='system') this.applyTheme(); });
      let rto;
      window.addEventListener('resize', ()=>{ clearTimeout(rto); rto=setTimeout(()=>{ if(this.view==='dashboard') this.renderDaily(); }, 200); });
      try{
        const r = await this.api('/admin/session');
        this.loggedIn = !!r.logged_in;
      }catch(e){ this.loggedIn = false; }
      this.ready = true;
      if(this.loggedIn) this.afterLogin();
    },

    /* ── 登录后进场：刷壳层数据 + 首屏页数据（走 enter，与后续切页同一入口） ── */
    afterLogin(){ this.loadHealth(); this.enter(this.view); },

    /* ═══ 软导航：切 view + 换 URL，不重载文档 ═══ */

    /* 切页统一入口：首次进入加载数据，重入策略见函数体内注释。 */
    enter(k){
      const p = PAGES[k];
      if(!p) return;
      if(this.entered[k]){
        // 已进入过的页默认保活，不重拉（滚动位置与已加载数据保留）。
        // growth / logs 由后端按天或持续改写（签到与上报链 22:00 补跑、日志追加、概览含「上次执行」），
        // 故以 realtime 声明进入即刷新；两次进入间隔 <5s 时退化为保活，抑制快速切页的重复请求。
        if(p.realtime && Date.now() - (this.entered[k] || 0) > 5000){
          this.entered[k] = Date.now();
          if(p.load) p.load.call(this);
        }
        return;
      }
      this.entered[k] = Date.now();
      if(p.load) p.load.call(this);
    },
    applyTitle(k){
      // 页名映照惰性采集：侧栏导航文案是页名唯一来源，但导航在主界面渲染后（它在
      // template x-if 里）才进 DOM，而改标题总是发生在渲染之后。
      // 采集为空（壳层此刻不在 DOM 里，例如会话过期后触发 popstate）时**不缓存**：
      // 否则空表会被永久记住，此后标题再也不更新。
      if(!this._titles || !Object.keys(this._titles).length){
        const m = Object.fromEntries([...document.querySelectorAll('.nav-link')]
          .map(a=>[pageKey(a.getAttribute('href')), a.textContent.trim()]));
        if(Object.keys(m).length) this._titles = m;
      }
      const title = this._titles && this._titles[k];
      if(!title) return;
      document.title = document.title.replace(/( · .*)?$/, ' · ' + title);
    },
    /* 浏览器前进/后退（由 shell 的 @popstate.window 转发）：只有经 Alpine 求值，this 才是
       响应式代理；若在 app() 里直接监听并给原始对象赋值，view 变了也不会触发重渲染。 */
    onPop(e){
      const k = (e.state && e.state.view) || pageKey(location.pathname);
      if(!PAGES[k] || k === this.view) return;   // 同页返回：保留当前页（表单输入、滚动位置都在）
      this.mobileNav = false;                    // 换页了：收起移动端侧栏（与 navigate 一致）
      this.view = k;
      this.applyTitle(k);
      this.enter(k);
      // 必须等 Alpine 把新 view 显示出来再滚：否则此刻文档还停在上一页的高度，
      // 滚动位置会被浏览器夹到 0（x-show 的显隐是在下一微任务里生效的）。
      const y = (e.state && e.state.scroll) || 0;
      this.$nextTick(()=>requestAnimationFrame(()=>window.scrollTo(0, y)));
    },

    navigate(k){
      if(!PAGES[k] || k === this.view) return;
      this.mobileNav = false; // 移动端点导航后必须收起侧栏，否则遮罩一直盖着内容
      history.replaceState(Object.assign({}, history.state, {scroll: window.scrollY}), '');
      this.view = k;
      history.pushState({view:k}, '', k === 'dashboard' ? '/' : '/' + k);
      this.applyTitle(k);
      window.scrollTo(0, 0);
      this.enter(k);
    },
    /* 侧边栏 <a href> 的拦截层：href 是唯一真相，未知目标一律交回浏览器（404 语义不变） */
    navClick(e){
      if(e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return; // 新标签/新窗口：交给浏览器
      const k = pageKey(e.currentTarget.getAttribute('href'));
      if(!PAGES[k]) return;
      e.preventDefault();
      this.navigate(k);
    },
    /* 程序化跳转（如任务页「前往登录」）；不再整页跳转，因此现在可以直接跨页调用 */
    go(v){ this.navigate(pageKey(v)); },

    /* ── API / 基础工具 ── */
    async api(path, opts={}){
      opts.headers = Object.assign({'Content-Type':'application/json'}, opts.headers||{});
      const r = await fetch(path, opts);
      // 登录端点自身的 401 是「密码错误」，不是会话过期：交下方按 data.error 报出服务端原文。
      // 若一并当作会话过期，用户看到的是「登录已过期」，而服务端返回的「密码错误」被吞掉。
      if(r.status===401 && path!=='/admin/login'){ this.loggedIn=false; this.resetSession(); throw new Error('登录已过期'); }
      const data = await r.json().catch(()=>({}));
      if(!r.ok) throw new Error(data.error||('HTTP '+r.status));
      return data;
    },
    /* 会话边界：清空「已进场」缓存。401 或退出后再登录属新会话，数据已失效；
       若不清理，afterLogin() → enter(view) 会被 entered 挡下，当前页保留过期前的旧统计 / 旧 Key 列表。 */
    resetSession(){ this.entered = {}; },
    toast(msg, type=''){
      const id = ++this._toastSeq;
      this.toasts.push({id,msg,type});
      setTimeout(()=>{ this.toasts = this.toasts.filter(t=>t.id!==id); }, 3400);
    },
    busy(fn){
      return async (...a)=>{ this.loading=true; try{ return await fn(...a); }
        catch(e){ this.toast(e.message,'err'); }
        finally{ this.loading=false; } };
    },
    /**
     * 通用确认弹窗。
     * opts.detail  次要说明（灰字，排在正文下方）
     * opts.okText  主按钮文案，默认「确定」
     * opts.danger  是否危险操作（默认 true，走 btn-danger；非破坏性操作传 false 走主色）
     */
    askConfirm(title, message, fn, opts){
      const o = opts || {};
      this.confirmBox = {
        open:true, title, message,
        detail:o.detail||'', okText:o.okText||'确定', danger:o.danger!==false,
        onOk:()=>{ this.confirmBox.open=false; fn(); },
      };
    },
    copy(text){
      navigator.clipboard.writeText(text).then(()=>this.toast('已复制到剪贴板','ok'));
    },
    fmt(n){ return (n||0).toLocaleString(); },
    compact(n){
      n = n||0;
      if(n>=1e9) return (n/1e9).toFixed(1).replace(/\.0$/,'')+'B';
      if(n>=1e6) return (n/1e6).toFixed(1).replace(/\.0$/,'')+'M';
      if(n>=1e3) return (n/1e3).toFixed(1).replace(/\.0$/,'')+'K';
      return String(n);
    },
    ts(unix, withSec){
      if(!unix) return '—';
      const d = typeof unix==='number' ? new Date(unix*1000) : new Date(unix);
      if(isNaN(d.getTime())) return String(unix);
      const p = n=>String(n).padStart(2,'0');
      let s = d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes());
      if(withSec) s += ':'+p(d.getSeconds());
      return s;
    },
    pct(a){
      if(!a.capacity_size) return 0;
      return Math.max(0, Math.min(100, a.capacity_remain/a.capacity_size*100)).toFixed(1);
    },
    fmtDosage(v){
      if(v==null) return '—';
      return typeof v==='number' ? String(+v.toFixed(2)) : v;
    },

    /* ── 登录 ── */
    async doLogin(){
      if(!this.password){ this.toast('请输入密码','err'); return; }
      this.loading = true;
      try{
        await this.api('/admin/login',{method:'POST',body:JSON.stringify({password:this.password})});
        this.loggedIn = true; this.password = '';
        this.toast('登录成功','ok');
        this.afterLogin();
      }catch(e){ this.toast(e.message,'err'); }
      this.loading = false;
    },
    async logout(){
      try{ await this.api('/admin/logout',{method:'POST'}); }catch(e){}
      this.loggedIn = false;
      this.resetSession();
      location.href = '/';
    },

    /* ── 跨页数据加载器 ── */
    /* ── 健康状态 / 统计 ── */
    async loadHealth(){
      try{ this.health = await fetch('/health').then(r=>r.json()); }catch(e){}
    },
    /* ── Keys ── */
    // loadKeys 归「密钥」页层（pages/keys.js）；统计页计数与日志页 Key 下拉按需调用，
    // 共享层不再保留同名实现，避免两份实现静默漂移。
    /* ── 模型 ── */
    async loadModels(){
      this.models.loading = true;
      try{
        const r = await this.api('/admin/models');
        this.models.models = r.models||[];
        this.models.ids = r.ids||[];
        this.models.state = r.state||null;
      }catch(e){ /* 静默，设置页不走这里 */ }
      finally{ this.models.loading = false; }
    },
    async loadSettings(){
      try{ this.settings = await this.api('/admin/settings'); this._savedSettings = Object.assign({}, this.settings); }catch(e){}
    },

    /** 上报条数区间文案：有波动显示 N±J（实际 a-b 条），无波动显示 N 条 */
    growthCountText(){
      const s = this.settings||{};
      const n = Math.min(10, Math.max(1, +s.growth_report_count||10));
      const j = Math.min(10, Math.max(0, +s.growth_report_jitter||0));
      if(!j) return n+' 条';
      return n+'±'+j+' 条（实际 '+(Math.max(1,n-j))+'-'+(Math.min(10,n+j))+' 条随机）';
    },
    growthCountHint(){
      const s = this.settings||{};
      const j = Math.min(10, Math.max(0, +s.growth_report_jitter||0));
      return j
        ? '上报 '+this.growthCountText()+'，每次执行在范围内随机取一个整数（更像真人，降低风控判定）；同会话多轮上报每条间隔 1.5 秒'
        : '上报 '+this.growthCountText()+'（1-10 条，填 0 波动数则固定不变）；同会话多轮上报每条间隔 1.5 秒';
    },
  };
}

/* ═══ 总装：共享层 + 当前页层（同名成员以页面层为准） ═══ */
/* 页面 key 来自服务端渲染的 <body data-page>；缺失时回退首页。 */
function app(){
  const key = pageKey(document.body.dataset.page);
  const page = PAGES[key];
  if(!page) console.warn("未注册的页面："+key);
  const inst = appShell();
  // 合并全部页面层（不只当前页）：7 个 view 常驻同一文档，Alpine 会求值全部页面的
  // 指令，而它始终在 x-data 根作用域（即 inst）上求值 —— 只合并当前页会让其他页的
  // 表达式报 ReferenceError。同名成员以后注册者胜出（PAGES 键序即文件拼接序），
  // 当前仅 load() / loadKeys() 同名且两侧实现一致；共享层不得再放单页专用加载器。
  // 用属性描述符而非 Object.assign：页面层的 getter 必须保持 getter（Object.assign
  // 会立即求值、且以页面对象为 this，那里没有 stats/resources 等状态）。
  for(const id of Object.keys(PAGES)){
    for(const k of Object.keys(PAGES[id])){
      Object.defineProperty(inst, k, Object.getOwnPropertyDescriptor(PAGES[id], k));
    }
  }
  // view 必须在 Alpine 首次求值前就绪：它决定哪个 .view 显示（错的第一帧会闪出其余页面）。
  // entered 必须留空：afterLogin() 走的就是 enter(this.view)，预置首屏 key 会让首屏页的
  // load() 被直接跳过，且此后切回该页也不再加载（entered 已为真）——首屏永远是空态。
  // （resetSession() 同样清空 entered。）
  inst.view = key;
  inst.entered = {};
  return inst;
}
