/* ═══ 页面层：密钥（PAGE(key, def) 注册本页专属状态与动作） ═══ */
PAGE('keys', {
  /* ── 调用示例（纯展示：不发请求、不读状态） ──

     用 location.origin 而非 location.href / pathname 推导，原因由本项目结构决定：

       - 软导航只 pushState 改 path（/ → /keys），origin 恒定；
       - 全部 7 个 view 常驻同一文档，Alpine 对**每个** view 的表达式都求值，
         包括当前未显示的那个。故任何基于 pathname 反推前缀的写法，在密钥页以外的
         页面上都会被求值出错误的 base。origin 与当前路径无关，恒等于用户实际
         访问的地址（协议 + host + 端口），因此天然覆盖改端口、局域网 IP、HTTPS 反代。

     盲区：反代到子路径（https://host/buddy/ → 服务根）时正确 base 应带 /buddy 前缀，
     而 origin 拿不到。但全站路由硬编码（/v1/*、/assets/*、308 规范化到 /keys），
     本就不支持子路径部署，属既有边界而非本次引入。 */
  get baseUrl(){ return location.origin + '/v1'; },
  get chatUrl(){ return location.origin + '/v1/chat/completions'; },
  get modelsUrl(){ return location.origin + '/v1/models'; },
  /* 示例 Key 一律用占位符：代码块是会被截图、贴进 issue 与聊天记录的传播面，
     填真实 Key 等于把凭证贴出去。表格里已明文列出各 Key，替换成本一步。 */
  get sampleKey(){ return 'sk-xxxxxx'; },
  get curlSample(){
    return `curl ${this.chatUrl} \\
  -H "Authorization: Bearer ${this.sampleKey}" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"auto","stream":true,"messages":[{"role":"user","content":"你好"}]}'`;
  },

    /* ── Keys ── */
  async loadKeys(){
    try{ const r = await this.api('/admin/api-keys'); this.keys = r.keys||[]; }catch(e){}
  },
  async createKey(){
    this.busy(async()=>{
      const f = this.keyForm;
      await this.api('/admin/api-keys',{method:'POST',body:JSON.stringify({
        name:f.name, custom_key:f.custom_key })});
      this.toast('Key 已创建','ok');
      this.keyModal = false;
      this.keyForm = {name:'',custom_key:''};
      this.loadKeys();
    })();
  },
  // 仅就地替换受影响的一行，不整表重拉：
  // 行内备注经 x-model 双向绑定，整表替换会丢弃用户在其他行未提交的编辑。
  // 成功与失败分支均走行级替换，且都不得调用 loadKeys()。
  async updateKey(k, patch){
    const i = this.keys.findIndex(x=>x.id===k.id);
    if(i < 0) return;                  // 行已被其他操作移除（刷新 / 删除）：无可就地替换的目标
    const before = Object.assign({}, this.keys[i]);  // 失败回滚基准（请求前的服务端值）
    try{
      const r = await this.api('/admin/api-keys/'+k.id,{method:'PUT',body:JSON.stringify(patch)});
      // updateKey 回传 {key}：以服务端返回值为准，避免本地浮点 / 裁剪与服务端不一致
      const row = (r && r.key) ? r.key : Object.assign({}, before, patch);
      const j = this.keys.findIndex(x=>x.id===k.id);
      if(j >= 0) this.keys.splice(j, 1, row);
      this.toast('已更新','ok');
    }catch(e){
      // 失败：将该行回滚为请求前的服务端值，用户本次编辑丢弃。
      // 不可保留 patch：输入框会停留在服务端已拒绝的值上，重试仍会失败；
      // 不可整表重拉（loadKeys）：会一并丢弃其他行未提交的编辑。
      const j = this.keys.findIndex(x=>x.id===k.id);
      if(j >= 0) this.keys.splice(j, 1, Object.assign({}, before));
      this.toast(e.message,'err');
    }
  },
  deleteKey(k){
    this.askConfirm('删除 Key','确定删除该 Key？此操作不可撤销。', ()=>{
      this.busy(async()=>{
        await this.api('/admin/api-keys/'+k.id,{method:'DELETE'});
        this.toast('已删除','ok'); this.loadKeys();
      })();
    });
  },

  /* ── 进场：本页所需数据 ── */
  load(){ this.loadKeys(); },
});
