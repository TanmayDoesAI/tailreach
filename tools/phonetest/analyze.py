"""Per-second table and summary of a phone stats recording (record.py).

    python3 tools/phonetest/analyze.py [path-to-viewer-stats.jsonl]
"""
import json,sys,os,statistics as st,datetime as dt
F=sys.argv[1] if len(sys.argv)>1 else os.path.expanduser('~/Library/Application Support/mydevices/viewer-stats.jsonl')
rows=[json.loads(l) for l in open(F)]
ph=[r for r in rows if r['src']=='phone']; mac=[r for r in rows if r['src']=='mac']
def macat(ts):
    return min(mac,key=lambda m:abs(m['ts']-ts)) if mac else {}
out=[]; prev=None
for r in ph:
    if prev and 'framesDecoded' in r and 'framesDecoded' in prev and r['framesDecoded']>=prev['framesDecoded']:
        d=lambda k: r.get(k,0)-prev.get(k,0)
        dec=d('framesDecoded'); em=d('jitterBufferEmittedCount')
        m=macat(r['ts'])
        out.append(dict(t=dt.datetime.fromtimestamp(r['ts']/1000).strftime('%H:%M:%S'),
          shown=r.get('shown',0), dec=dec, drop=d('framesDropped'), frz=d('freezeCount'), frzms=round(d('totalFreezesDuration')*1000),
          jb=round(d('jitterBufferDelay')/em*1000) if em>0 else None, jbt=round(r.get('jitterBufferTargetDelay',0)/max(r.get('jitterBufferEmittedCount',1),1)*1000),
          decms=round(d('totalDecodeTime')/dec*1000,1) if dec>0 else None, proc=round(d('totalProcessingDelay')/dec*1000) if dec>0 else None,
          lost=d('packetsLost'), nack=d('nackCount'), pli=d('pliCount'), kbps=round(d('bytesReceived')*8/1000),
          rtt=round(r.get('pairRtt',0)*1000), res=f"{r.get('frameWidth')}x{r.get('frameHeight')}",
          br=m.get('br',0)//1000, act=m.get('act',0)//1000, bl=m.get('bl',0), mdrop=m.get('drops',0), vis=r.get('vis')))
    prev=r
print(' time     shown dec drop frz frzms  jb  dec_ms proc lost nack pli  kbps rtt   br_k  act_k  bl mdrop res')
for o in out:
    print(f"{o['t']} {o['shown']:4} {o['dec']:4} {o['drop']:3} {o['frz']:3} {o['frzms']:5} {str(o['jb']):>4} {str(o['decms']):>6} {str(o['proc']):>4} {o['lost']:4} {o['nack']:4} {o['pli']:3} {o['kbps']:5} {o['rtt']:3} {o['br']:6} {o['act']:5} {o['bl']:3} {o['mdrop']:4} {o['res']} {o['vis'] if o['vis']!='visible' else ''}")
v=[o for o in out if o['vis']=='visible']
q=lambda xs,p: sorted(xs)[int(p*(len(xs)-1))] if xs else None
print('\nSUMMARY over',len(v),'s visible')
for k in ['shown','dec','kbps','rtt','bl']:
    xs=[o[k] for o in v if o[k] is not None]; print(f" {k:6} p5={q(xs,.05)} p50={q(xs,.5)} p95={q(xs,.95)}")
for k in ['jb','decms','proc']:
    xs=[o[k] for o in v if o[k] is not None]; print(f" {k:6} p50={q(xs,.5)} p95={q(xs,.95)} max={max(xs) if xs else None}")
print(' freezes',sum(o['frz'] for o in v),'total ms',sum(o['frzms'] for o in v),'| secs with shown<20:',sum(1 for o in v if o['shown']<20),'| lost',sum(o['lost'] for o in v),'nack',sum(o['nack'] for o in v),'pli',sum(o['pli'] for o in v),'| mac drops',sum(o['mdrop'] for o in v))
