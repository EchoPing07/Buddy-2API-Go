/* ═══ 页面层：密钥（PAGE(key, def) 注册本页专属状态与动作） ═══ */
PAGE('keys', {
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
