/* ═══ 页面层：实验（PAGE(key, def) 注册本页专属状态与动作） ═══ */
PAGE('labs', {
  /* ── 嵌入模型示例（纯展示；开关状态 labs.embeddings_enabled 放壳层） ──
     用 location.origin 推导端点的原因见 pages/keys.js（全 view 常驻，pathname 不可靠）。 */
  get embUrl(){ return location.origin + '/v1/embeddings'; },
  /* 示例 Key 一律用占位符（同密钥页）：代码块会被截图外传，填真实 Key 等于泄凭证。 */
  get sampleKey(){ return 'sk-xxxxxx'; },
  /* 嵌入模型当前仅 bge-m3（上游嵌入注册表实测唯一可用项，见 /v1/embeddings 实现注释） */
  get embCurlSample(){
    return `curl ${this.embUrl} \\
  -H "Authorization: Bearer ${this.sampleKey}" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"bge-m3","input":["你好，世界"]}'`;
  },

  /* ── 号池：客户端接入示例 ──
     pi 默认既不发会话头也不发 prompt_cache_key，裸连只能靠「首条 user 文本」派生会话键，
     而上下文压缩会让首条 user 消失 → 粘性被打断。把这段 compat 粘进 models.json 就 100% 稳定。 */
  get poolModelsJsonSample(){
    return `{
  "providers": {
    "buddy": {
      "baseUrl": "${location.origin}/v1",
      "api": "openai-completions",
      "apiKey": "sk-xxxxxx",
      "compat": {
        "sendSessionAffinityHeaders": true,
        "sessionAffinityFormat": "openai"
      },
      "models": [{ "id": "glm-5.3" }]
    }
  }
}`;
  },
  /* 号池统计走共享层的 pool（app.js）；本页不重复定义，否则会顶掉共享实现
     （web_test 的 TestPageLayerDoesNotShadowShell 锁定了这一点）。 */

  async loadLabs(){
    try{
      const r = await this.api('/admin/experiments');
      this.labs.embeddings_enabled = !!r.embeddings_enabled;
      this.labs.pool_enabled = !!r.pool_enabled;
      this.labs.pool_session_ttl = r.pool_session_ttl;
      this.labs.pool_max_retry = r.pool_max_retry;
      this.labs.pool_cool_max_min = r.pool_cool_max_min;
      this.labs.pool_probe_cron = r.pool_probe_cron;
    }catch(e){}
    // 只有开启时才拉号池状态（关闭时统计行整块不显示，拉了也没地方用）
    if(this.labs.pool_enabled) this.loadPoolSummary();
  },
  /* 开关即时生效（无「保存」按钮）：非乐观更新，以服务端回值为准；
     失败由 busy() 统一弹错，开关停在原状态，不会出现 UI 与服务端不一致。 */
  toggleEmbeddings(){
    const next = !this.labs.embeddings_enabled;
    this.busy(async()=>{
      const r = await this.api('/admin/experiments',{method:'PUT',body:JSON.stringify({embeddings_enabled:next})});
      this.labs.embeddings_enabled = !!r.embeddings_enabled;
      this.toast(r.embeddings_enabled ? '嵌入端点已开启' : '嵌入端点已关闭','ok');
    })();
  },

  /* ── 号池开关 ──
     关闭时必须确认：关闭会立刻让所有在跑的会话回到单账号，用户要知道这一点。 */
  togglePool(){
    const next = !this.labs.pool_enabled;
    if(next){
      this.busy(async()=>{
        const r = await this.api('/admin/experiments',{method:'PUT',body:JSON.stringify({pool_enabled:true})});
        this.applyExperiments(r);
        this.toast('号池负载均衡已开启','ok');
        this.loadPoolSummary();
        this.loadAccounts();   // 账号页的池状态列
      })();
      return;
    }
    this.askConfirm('关闭号池负载均衡',
      '关闭后 /v1/chat/completions 立刻回到「只走当前活动账号」，全部会话重新绑定到该账号。',
      ()=>this.busy(async()=>{
        const r = await this.api('/admin/experiments',{method:'PUT',body:JSON.stringify({pool_enabled:false})});
        this.applyExperiments(r);
        this.pool = null;
        this.toast('号池已关闭','ok');
      })(), {okText:'关闭', danger:true});
  },
  /* 把服务端回值灌回 labs（PUT 恒回服务端真值，env 锁定时会与用户意图不同） */
  applyExperiments(r){
    this.labs.embeddings_enabled = !!r.embeddings_enabled;
    this.labs.pool_enabled = !!r.pool_enabled;
    this.labs.pool_session_ttl = r.pool_session_ttl;
    this.labs.pool_max_retry = r.pool_max_retry;
    this.labs.pool_cool_max_min = r.pool_cool_max_min;
    this.labs.pool_probe_cron = r.pool_probe_cron;
  },
  /* 数值参数保存（blur 触发；非法值不提交并提示，避免把手误敲进配置） */
  savePoolParam(field, min, max, label){
    const v = Number(this.labs[field]);
    if(!Number.isFinite(v) || v < min || v > max || !Number.isInteger(v)){
      this.toast(label+' 需为 '+min+'-'+max+' 的整数','err');
      this.loadLabs();   // 回滚输入框到服务端真值
      return;
    }
    const body = {}; body[field] = v;
    this.busy(async()=>{
      const r = await this.api('/admin/experiments',{method:'PUT',body:JSON.stringify(body)});
      this.applyExperiments(r);
      this.toast(label+' 已更新','ok');
    })();
  },
  savePoolCron(){
    const v = String(this.labs.pool_probe_cron||'').trim();
    if(v.split(/\s+/).length !== 6){
      this.toast('探测 cron 需为 6 段含秒（如 0 0 0,6,12,18 * * *）','err');
      return;
    }
    this.busy(async()=>{
      const r = await this.api('/admin/experiments',{method:'PUT',body:JSON.stringify({pool_probe_cron:v})});
      this.applyExperiments(r);
      this.toast('探测时间已更新','ok');
    })();
  },

  /* ── 号池统计（实验页只看概要，明细在账号页） ── */
  async loadPoolSummary(){
    try{ this.pool = await this.api('/admin/pool'); }catch(e){ this.pool = null; }
  },
  /* 粘性命中率：判断 TTL 该调大还是调小的唯一依据 */
  poolHitRate(){
    const p = this.pool;
    if(!p) return '—';
    const total = (p.sticky_hit_total||0) + (p.sticky_miss_total||0);
    if(!total) return '—';
    return Math.round((p.sticky_hit_total||0)/total*100) + '%';
  },
  poolEnabledDays(){
    const at = this.pool && this.pool.enabled_at;
    if(!at) return '';
    const d = Math.floor((Date.now()/1000 - at)/86400);
    return d > 0 ? ('已开启 '+d+' 天') : '今天开启';
  },

  /* ── 进场：本页所需数据 ── */
  load(){ this.loadLabs(); },
});
