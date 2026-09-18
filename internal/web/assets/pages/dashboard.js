/* ═══ 页面层：统计（PAGE(key, def) 注册本页专属状态与动作） ═══ */
PAGE('dashboard', {
  /* ── 统计 ── */
  get successRate(){
    const t = this.stats.total_requests||0;
    if(!t) return null;
    return (100*(1-(this.stats.error_count||0)/t)).toFixed(1);
  },
  get avg14(){
    const d = (this.stats.daily||[]).slice(-14);
    if(!d.length) return 0;
    return Math.round(d.reduce((s,x)=>s+(x.requests||0),0)/d.length);
  },
  get maxReq(){
    // 不使用展开运算：模型数量增长时 Math.max(1, ...arr) 会触及参数个数上限
    return (this.stats.by_model||[]).reduce((a,m)=>Math.max(a,m.requests||0),1);
  },
  async loadStats(){
    try{
      this.stats = await this.api('/admin/stats');
      this.$nextTick(()=>requestAnimationFrame(()=>this.renderDaily()));
    }catch(e){ this.toast(e.message,'err'); }
  },

    /* ── 图表：近 14 天（手写 SVG，双轴：柱=Tokens，线=请求·Catmull-Rom 平滑曲线） ── */
  renderDaily(){
    const el = document.getElementById('chartDaily');
    if(!el) return;
    const daily = (this.stats.daily||[]).slice(-14);
    if(!daily.length){ el.innerHTML = '<div class="chart-empty">暂无数据</div>'; return; }
    const W = el.clientWidth || 600;
    if(W < 40) return;
    const H = 236, padL = 34, padR = 44, padT = 12, padB = 24;
    const iw = W-padL-padR, ih = H-padT-padB;
    const maxB = Math.max(...daily.map(d=>d.tokens||0), 1);   // 柱：Tokens（左轴）
    const maxL = Math.max(...daily.map(d=>d.requests||0), 1); // 线：请求（右轴）
    const step = iw/daily.length;
    const barW = Math.max(4, Math.min(16, step*0.48));
    const cx = i => padL + step*i + step/2;
    const clampY = v => Math.max(padT+1, Math.min(padT+ih-1, v));
    const yB = v => padT + ih - v/maxB*ih;
    const yL = v => padT + ih - v/maxL*ih;
    // Catmull-Rom 转三次贝塞尔：相邻点作控制点，折线平滑过渡；控制点钳制在绘图区内防过冲
    const smoothPath = pts => {
      if(!pts.length) return '';
      if(pts.length===1) return `M${pts[0][0].toFixed(1)},${pts[0][1].toFixed(1)}`;
      let d = `M${pts[0][0].toFixed(1)},${pts[0][1].toFixed(1)}`;
      for(let i=0;i<pts.length-1;i++){
        const p0=pts[Math.max(0,i-1)], p1=pts[i], p2=pts[i+1], p3=pts[Math.min(pts.length-1,i+2)];
        d += `C${(p1[0]+(p2[0]-p0[0])/6).toFixed(1)},${clampY(p1[1]+(p2[1]-p0[1])/6).toFixed(1)}`
           + ` ${(p2[0]-(p3[0]-p1[0])/6).toFixed(1)},${clampY(p2[1]-(p3[1]-p1[1])/6).toFixed(1)}`
           + ` ${p2[0].toFixed(1)},${p2[1].toFixed(1)}`;
      }
      return d;
    };
    let s = '';
    for(let g=0; g<=4; g++){
      const gy = padT + ih - ih*g/4;
      s += `<line class="grid-line" x1="${padL}" x2="${W-padR}" y1="${gy.toFixed(1)}" y2="${gy.toFixed(1)}"/>`;
      s += `<text class="axis-label" text-anchor="end" x="${padL-7}" y="${(gy+3.5).toFixed(1)}">${this.compact(Math.round(maxB*g/4))}</text>`;
      s += `<text class="axis-label" x="${W-padR+7}" y="${(gy+3.5).toFixed(1)}">${this.compact(Math.round(maxL*g/4))}</text>`;
    }
    daily.forEach((d,i)=>{
      const y = yB(d.tokens||0);
      s += `<rect class="bar-c" data-i="${i}" x="${(cx(i)-barW/2).toFixed(1)}" y="${y.toFixed(1)}" width="${barW.toFixed(1)}" height="${Math.max(0,padT+ih-y).toFixed(1)}" rx="${Math.min(3,barW/2).toFixed(1)}"/>`;
    });
    const linePts = daily.map((d,i)=>[cx(i), clampY(yL(d.requests||0))]);
    s += `<path class="tok-line" d="${smoothPath(linePts)}"/>`;
    linePts.forEach(p=>{ s += `<circle class="tok-dot" cx="${p[0].toFixed(1)}" cy="${p[1].toFixed(1)}" r="2.6"/>`; });
    const k = Math.max(1, Math.ceil(daily.length/7));
    daily.forEach((d,i)=>{
      if(i%k===0 || i===daily.length-1) s += `<text class="axis-label" text-anchor="middle" x="${cx(i).toFixed(1)}" y="${H-6}">${(d.date||'').slice(5)}</text>`;
    });
    daily.forEach((d,i)=>{ s += `<rect class="hz" data-i="${i}" x="${(padL+step*i).toFixed(1)}" y="${padT}" width="${step.toFixed(1)}" height="${ih.toFixed(1)}" fill="transparent"/>`; });
    s += `<g class="tip" opacity="0" pointer-events="none"><rect class="tip-bg" rx="7" width="130" height="70"/><text class="tip-t" x="10" y="16"></text><text class="tip-l" x="10" y="30"></text><text class="tip-l" x="10" y="43"></text><text class="tip-l" x="10" y="56"></text></g>`;
    el.innerHTML = `<svg width="${W}" height="${H}" viewBox="0 0 ${W} ${H}">${s}</svg>`;
    this._chartGeom = {padL, step, W};
    // 悬停提示
    const svg = el.querySelector('svg');
    const tip = svg.querySelector('.tip');
    const bg = tip.querySelector('.tip-bg');
    const [tt, l1, l2, l3] = tip.querySelectorAll('text');
    svg.querySelectorAll('.hz').forEach(hz=>{
      hz.addEventListener('mouseenter', ()=>{
        const i = +hz.dataset.i, d = daily[i];
        tt.textContent = d.date||'';
        l1.textContent = `请求数 ${this.fmt(d.requests||0)}`;
        l2.textContent = `Tokens 用量 ${this.fmt(d.tokens||0)}`;
        l3.textContent = `Credits 消耗 ${this.fmt(Math.round((d.credit||0)*100)/100)}`;
        const w = Math.max(...[tt,l1,l2].map(t=>t.getComputedTextLength())) + 20;
        bg.setAttribute('width', w.toFixed(0));
        const g = this._chartGeom;
        let tx = g.padL + g.step*i + g.step + 8;
        if(tx + w > g.W - 2) tx = g.padL + g.step*i - w - 8;
        tip.setAttribute('transform', `translate(${tx.toFixed(1)},${padT+2})`);
        tip.setAttribute('opacity','1');
        const bar = svg.querySelector(`.bar-c[data-i="${i}"]`);
        if(bar) bar.classList.add('hl');
      });
      hz.addEventListener('mouseleave', ()=>{
        tip.setAttribute('opacity','0');
        svg.querySelectorAll('.bar-c.hl').forEach(b=>b.classList.remove('hl'));
      });
    });
  },

  /* ── 模型 ── */
  get modelChips(){
    const arr=this.models.models||[];
    if(!arr.length) return (this.models.ids||[]).map(id=>({id,name:id,disp:id,credits:'',promo:''}));
    // 同名模型消歧：显示名重复时附上 id，如 Hy3(hy3) / Hy3(hy3-x)
    const label=m=>(m.name&&m.name!==m.id)?m.name:m.id;
    const cnt={};
    arr.forEach(m=>{const l=label(m);cnt[l]=(cnt[l]||0)+1});
    return arr.map(m=>{const l=label(m);return{id:m.id,name:l,disp:cnt[l]>1?l+'('+m.id+')':l,credits:m.credits||'',promo:m.promo||''}});
  },
  get modelsStateText(){
    const m = this.models.state;
    if(!m) return '';
    return (m.fallback?'内置回退表':'实时列表')+' · '+(m.count??0)+' 个模型'+(m.fetched_at?' · 更新于 '+this.ts(m.fetched_at):'');
  },

  async refreshModels(){
    this.busy(async()=>{
      const r = await this.api('/admin/models/refresh',{method:'POST'});
      this.toast('模型列表已刷新（'+(r.ids||[]).length+' 个）','ok');
      this.loadModels();
    })();
  },

  /* ── 进场：本页所需数据 ── */
  load(){ this.loadStats(); this.loadModels(); this.loadKeys(); },
});
