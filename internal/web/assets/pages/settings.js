/* ═══ 页面层：设置（PAGE(key, def) 注册本页专属状态与动作） ═══ */
PAGE('settings', {
  /* ── 设置 ── */
  async saveSettings(){
    this.busy(async()=>{
      const s = this.settings;
      // 数字输入被清空时 Alpine 的 x-model.number 得到 null（+null === 0）。
      // 后端对这些字段的语义分两种：>0 才写入（静默忽略 0）/ 硬校验 1..3600。
      // 统一口径：留空 = 不下发该键（保持服务端原值），避免「发 0」导致静默丢弃或整单 400；
      // chat_timeout 是硬校验字段，单独就地报错，不发必然 400 的请求。
      const num = v => { const n = +v; return Number.isFinite(n) ? n : 0; };
      const body = {
        listen:(s.listen||'').trim(),
        region:s.region, auto_checkin:!!s.auto_checkin,
        checkin_mode:s.checkin_mode||'fixed',
        checkin_fallback:!!s.checkin_fallback,
        auto_growth:!!s.auto_growth,
        growth_report_jitter:Math.min(10, Math.max(0, num(s.growth_report_jitter))),
      };
      // cron/随机窗口仅在对应模式/开关下下发：存量非法值（常见来源是 env 写错）
      // 不应阻断改密码/改日志等无关保存；关闭时不下发，后端会校验存量值并回落默认。
      // 这样字段始终可见可改（x-show 而非 x-if），不会出现「改不了」的死锁。
      if(s.auto_checkin && (s.checkin_mode||'fixed')!=='random'){
        body.checkin_cron = (s.checkin_cron||'').trim();
      }
      if((s.checkin_mode||'fixed')==='random'){
        body.checkin_random_start = s.checkin_random_start||'';
        body.checkin_random_end = s.checkin_random_end||'';
      }
      if(s.auto_growth){
        body.growth_report_cron = (s.growth_report_cron||'').trim();
        body.growth_travel_cron = (s.growth_travel_cron||'').trim();
      }
      // 留空/非正不下发（后端这些字段是「>0 才写入」，发 0 会被静默忽略）
      if(num(s.growth_report_count) > 0)    body.growth_report_count = num(s.growth_report_count);
      if(num(s.resource_cache_seconds) > 0) body.resource_cache_seconds = num(s.resource_cache_seconds);
      if(num(s.log_retention_days) > 0)     body.log_retention_days = num(s.log_retention_days);
      if(num(s.log_max_size_mb) > 0)        body.log_max_size_mb = num(s.log_max_size_mb);
      const cto = num(s.chat_timeout_seconds);
      if(cto < 1 || cto > 3600){
        this.toast('Chat 响应超时需在 1-3600 秒之间','err');
        return;
      }
      body.chat_timeout_seconds = cto;
      if(this.pwForm.old_password || this.pwForm.new_password){
        body.old_password = this.pwForm.old_password;
        body.new_password = this.pwForm.new_password;
      }
      // 先取保存前的两份值用于变更提示：listen 改了才写「需重启」，其余情况不给误导性后缀
      const prev = this._savedSettings || {};
      try{
        this.settings = await this.api('/admin/settings',{method:'PUT',body:JSON.stringify(body)});
      }catch(e){
        // 失败时回滚界面到服务端真实值，避免输入框残留导致下次仍失败；
        // toast 交给外层 busy() 统一弹（此处不重复 toast，避免弹两条）
        this.loadSettings();
        throw e;
      }
      this._savedSettings = Object.assign({}, this.settings);
      this.pwForm = {old_password:'',new_password:''};
      const listenChanged = !!(prev.listen && body.listen && prev.listen !== body.listen);
      this.toast('设置已保存'+(listenChanged?'，监听地址修改需手动重启服务生效':''),'ok');
      this.loadHealth();
    })();
  },

  /* ── 进场：本页所需数据 ── */
  load(){ this.loadSettings(); },
});
