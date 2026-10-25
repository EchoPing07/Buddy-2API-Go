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

  async loadLabs(){
    try{
      const r = await this.api('/admin/experiments');
      this.labs.embeddings_enabled = !!r.embeddings_enabled;
    }catch(e){}
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

  /* ── 进场：本页所需数据 ── */
  load(){ this.loadLabs(); },
});
