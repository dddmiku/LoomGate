async owner => {
  const key=Symbol.for('loomgate.probe.lease'),lease=window[key];
  if(!lease)return {ok:true};
  if(lease.owner!==owner)return {error:'desktop_ownership_lost'};
  lease.controller.abort();clearTimeout(lease.timer);
  let timer;
  await Promise.race([lease.pending,new Promise(resolve=>{timer=setTimeout(resolve,1000);})]);
  clearTimeout(timer);
  if(!lease.settled)return {error:'desktop_cleanup_uncertain'};
  delete window[key];return {ok:true};
}
