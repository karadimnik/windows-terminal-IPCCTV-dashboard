#!/usr/bin/env python3
"""Summarise measurement cells. Usage: analyze.py <datadir> [run ...]  -> writes <run>.summary.json, prints one markdown row per run.
Input per run (written by measure.ps1 / mpv-baseline.ps1): <run>.meta.json, <run>.samples.jsonl (CPU), <run>.gpu.jsonl (GPU counters),
<run>.jsonl (page getStats samples + events, browsers only)."""
import json, os, sys, glob, statistics as st

EXPECT_FPS = 20.0

def jl(p):
    out = []
    if os.path.exists(p):
        for line in open(p, encoding='utf-8-sig'):
            line = line.strip()
            if line:
                try: out.append(json.loads(line))
                except Exception: pass
    return out

def mean(xs): return round(sum(xs) / len(xs), 1) if xs else None
def mx(xs): return round(max(xs), 1) if xs else None

def cpu_block(samples, lo, hi):
    s = [x for x in samples if lo <= x['t'] <= hi]
    return dict(n=len(s), cpu_mean=mean([x['cpu_pct'] for x in s]), cpu_max=mx([x['cpu_pct'] for x in s]),
                thread_mean=mean([x['busiest_pct'] for x in s]), thread_max=mx([x['busiest_pct'] for x in s]),
                ws_mb=mean([x['ws_mb'] for x in s]), sys_cpu=mean([x['sys_cpu_pct'] for x in s if x.get('sys_cpu_pct') is not None]),
                rig_cpu=mean([x['rig_cpu_pct'] for x in s]), procs=mx([x['n'] for x in s]))

def gpu_block(gpu, lo, hi):
    s = [x for x in gpu if lo <= x['t'] <= hi]
    keys = {}
    for x in s:
        for k, v in x['gpu_mine'].items(): keys.setdefault(k, []).append(v)
    mine = {k: [mean(v), mx(v)] for k, v in keys.items() if k.split('|')[1] in ('videodecode', '3d', 'videoprocessing', 'copy') and max(v) > 0}
    keys = {}
    for x in s:
        for k, v in x['gpu_all'].items(): keys.setdefault(k, []).append(v)
    allk = {k: [mean(v), mx(v)] for k, v in keys.items() if k.split('|')[1] in ('videodecode', '3d')}
    return dict(n=len(s), mine=mine, all=allk)

def page_block(page, lo, hi):
    s = [x for x in page if x.get('ev') == 's' and lo <= x['t'] <= hi]
    ev = [x for x in page if x.get('ev') != 's']
    out = dict(tiles={}, vis=sorted({x['vis'] for x in s}), errors=[x for x in ev if x.get('ev') == 'error'][:3])
    if len(s) < 2: return out
    a, b = s[0], s[-1]; dt = (b['t'] - a['t']) / 1000.0
    ttff = {x['path']: x['ms'] for x in ev if x.get('ev') == 'ttff'}
    out['restarts'] = sum(1 for x in ev if x.get('ev') == 'restart')
    out['gather_ms'] = {x['path']: x.get('gather_ms') for x in ev if x.get('ev') == 'ttff'}
    for ta, tb in zip(a['tiles'], b['tiles']):
        va, vb = ta.get('v', {}), tb.get('v', {})
        d = lambda k: (vb.get(k, 0) - va.get(k, 0))
        dec = d('framesDecoded'); rec = d('framesReceived')
        row = dict(path=tb['path'], res=f"{vb.get('frameWidth')}x{vb.get('frameHeight')}", impl=vb.get('decoderImplementation'),
                   power_eff=vb.get('powerEfficientDecoder'), fps=round(dec / dt, 1) if dt else None,
                   dropped=d('framesDropped'), received=rec, decoded=dec, freezes=d('freezeCount'), freeze_s=round(d('totalFreezesDuration'), 2),
                   lost=d('packetsLost'), ttff_ms=ttff.get(tb['path']))
        if d('jitterBufferEmittedCount') > 0: row['jb_ms'] = round(1000 * d('jitterBufferDelay') / d('jitterBufferEmittedCount'), 1)
        if dec > 0 and 'totalDecodeTime' in vb: row['dec_ms'] = round(1000 * d('totalDecodeTime') / dec, 2)
        qa, qb = ta.get('vq'), tb.get('vq')
        if qa and qb: row['render_dropped'] = qb['dropped'] - qa['dropped']; row['render_total'] = qb['total'] - qa['total']
        aa, ab = ta.get('a', {}), tb.get('a', {})
        if ab: row['audio_level_last'] = ab.get('audioLevel'); row['audio_energy_d'] = round(ab.get('totalAudioEnergy', 0) - aa.get('totalAudioEnergy', 0), 5)
        out['tiles'][tb['path']] = row
    out['window_s'] = round(dt, 1)
    return out

def verdict(page):
    t = list(page.get('tiles', {}).values())
    if not t: return 'no data'
    worst_fps = min(x['fps'] or 0 for x in t); frz = sum(x['freezes'] for x in t)
    drop = max((x['dropped'] / x['received']) if x['received'] else 1 for x in t)
    rdrop = max((x.get('render_dropped', 0) / x['render_total']) if x.get('render_total') else 0 for x in t)
    if worst_fps >= 0.95 * EXPECT_FPS and frz == 0 and drop <= 0.01 and rdrop <= 0.02: return 'smooth'
    if worst_fps >= 0.75 * EXPECT_FPS and frz <= 3 and drop <= 0.05: return 'marginal'
    return 'unusable'

def summarize(d, run):
    meta = json.load(open(os.path.join(d, run + '.meta.json'), encoding='utf-8-sig'))
    cpu = jl(os.path.join(d, run + '.samples.jsonl')); gpu = jl(os.path.join(d, run + '.gpu.jsonl')); page = jl(os.path.join(d, run + '.jsonl'))
    lo, hi = meta['sample_start_ms'], meta['sample_end_ms']
    res = dict(meta=meta)
    if meta.get('hide_ms'):
        h = meta['hide_ms']; res['visible'] = dict(cpu=cpu_block(cpu, lo, h), gpu=gpu_block(gpu, lo, h), page=page_block(page, lo, h))
        res['hidden'] = dict(cpu=cpu_block(cpu, h + 6000, hi), gpu=gpu_block(gpu, h + 6000, hi), page=page_block(page, h + 6000, hi))
        res['hidden_page_vis'] = sorted({x['vis'] for x in page if x.get('ev') == 's' and x['t'] >= h + 4000})
    else:
        res['cpu'] = cpu_block(cpu, lo, hi); res['gpu'] = gpu_block(gpu, lo, hi); res['page'] = page_block(page, lo, hi)
        res['verdict'] = verdict(res['page'])
    lat = [v for x in page if x.get('ev') == 's' and lo <= x['t'] <= hi for t in x['tiles'] for v in t.get('lat', [])]
    r2c = [v for x in page if x.get('ev') == 's' and lo <= x['t'] <= hi for t in x['tiles'] for v in t.get('r2c', [])]
    if lat: res['latency_s'] = dict(n=len(lat), median=round(st.median(lat), 3), p10=round(sorted(lat)[len(lat) // 10], 3), p90=round(sorted(lat)[(9 * len(lat)) // 10], 3))
    if r2c: res['recv_to_callback_ms'] = dict(n=len(r2c), median=round(st.median(r2c), 1), p90=round(sorted(r2c)[(9 * len(r2c)) // 10], 1))
    json.dump(res, open(os.path.join(d, run + '.summary.json'), 'w'), indent=1)
    return res

def gpu_str(g):
    parts = []
    for k, (m, x) in sorted(g.items()):
        ad, eng = k.split('|'); ad = 'GTX1070Ti' if '1070' in ad else ('HD530' if '530' in ad else ad)
        parts.append(f"{ad}.{eng[:5] if eng != 'videodecode' else 'dec'} {m}/{x}")
    return '; '.join(parts) or '0'

def row(res):
    m = res['meta']; p = res.get('page') or res.get('visible', {}).get('page'); c = res.get('cpu') or res.get('visible', {}).get('cpu'); g = res.get('gpu') or res.get('visible', {}).get('gpu')
    t = list(p['tiles'].values()) if p and p.get('tiles') else []
    impl = sorted({str(x['impl']) for x in t}); fps = '/'.join(str(x['fps']) for x in t)
    drops = '/'.join(str(x['dropped']) for x in t); frz = '/'.join(str(x['freezes']) for x in t)
    return (f"| {m['run']} | {m['browser']} | {m['hw']} | {m['tiles']} | {c['cpu_mean']} ({c['cpu_max']}) | {c['thread_mean']} ({c['thread_max']}) | {c['ws_mb']} | "
            f"{gpu_str(g['mine'])} | {', '.join(impl)} | {fps} | {drops} | {frz} | {res.get('verdict', '')} |")

if __name__ == '__main__':
    d = sys.argv[1]; runs = sys.argv[2:] or sorted(os.path.basename(f)[:-10] for f in glob.glob(os.path.join(d, '*.meta.json')))
    for r in runs:
        try: print(row(summarize(d, r)))
        except Exception as e: print(f'| {r} | ERROR {e!r} |')
