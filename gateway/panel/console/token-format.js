'use strict';
// Shared by both consoles. Only presentation changes; ledger values stay intact.
(function(root){
  function numeric(value){
    if(typeof value==='string'){
      if(!value.trim())return null;
      value=Number(value.replace(/[, ]/g,''));
    }
    return typeof value==='number'&&Number.isFinite(value)&&value>=0?value:null;
  }
  function exact(value){
    const n=numeric(value);
    return n===null?'—':n.toLocaleString('zh-CN',{maximumFractionDigits:20});
  }
  function compact(value){
    let n=numeric(value);
    if(n===null)return '—';
    const units=['','K','M','B','T'];
    let unit=0;
    while(n>=1000&&unit<units.length-1){n/=1000;unit++;}
    n=Number(n.toFixed(2));
    // Rounding at a boundary must promote 999999 to 1M, never 1000K.
    if(n>=1000&&unit<units.length-1){n/=1000;unit++;}
    return String(n)+units[unit];
  }
  function html(value){
    const label=compact(value);
    return label==='—'?'—':'<span title="'+exact(value)+' tokens">'+label+'</span>';
  }
  const format={compact,exact,html};
  if(typeof module==='object'&&module.exports)module.exports=format;
  else root.LoomGateTokens=format;
})(typeof globalThis==='object'?globalThis:this);
