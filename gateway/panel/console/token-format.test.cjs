'use strict';
const assert=require('node:assert/strict');
const {test}=require('node:test');
const fs=require('node:fs');
const path=require('node:path');
const {compact,exact,html}=require('./token-format.js');

test('token counts use decimal units and promote rounded boundaries',()=>{
  for(const [value,want] of [[0,'0'],[999,'999'],[1000,'1K'],[1234,'1.23K'],
      [999994,'999.99K'],[999999,'1M'],[1234567,'1.23M'],[999999999,'1B'],
      [1234567890,'1.23B'],[999999999999,'1T'],['1,234,567','1.23M']]){
    assert.equal(compact(value),want,String(value));
  }
});
test('missing or invalid counts never turn into zero or executable markup',()=>{
  for(const value of [undefined,null,'',NaN,Infinity,-1,true,'<img src=x>']){
    assert.equal(compact(value),'—');
    assert.equal(exact(value),'—');
    assert.equal(html(value),'—');
  }
  assert.equal(html(0),'<span title="0 tokens">0</span>');
  assert.equal(html(1234567890),'<span title="1,234,567,890 tokens">1.23B</span>');
});
test('modern and legacy asset chains load the shared formatter first',()=>{
  const entry=fs.readFileSync(path.join(__dirname,'index.html'),'utf8');
  assert.ok(entry.indexOf('console/token-format.js')<entry.indexOf('console/app.js'));
  const legacy=fs.readFileSync(path.join(__dirname,'..','index.html'),'utf8');
  assert.ok(legacy.includes(fs.readFileSync(path.join(__dirname,'token-format.js'),'utf8').trim()));
  assert.ok(legacy.indexOf('root.LoomGateTokens=format')<legacy.indexOf('function compactNum'));
});
