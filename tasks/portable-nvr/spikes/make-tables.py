#!/usr/bin/env python3
"""Builds the report tables from <datadir>/*.summary.json. Usage: make-tables.py <datadir> > tables.md"""
import json, glob, os, sys
d = sys.argv[1]
S = {os.path.basename(f)[:-13]: json.load(open(f)) for f in glob.glob(os.path.join(d, '*.summary.json'))}

def g(res, kind, eng, adapter='1070'):
    blk = res['gpu'][kind]
    for k, v in blk.items():
        if adapter in k and k.endswith('|' + eng): return v[0]
    return 0.0

def impl_of(browser, hw, t):
    for pre in ('I2', 'I'):
        r = S.get(f'{pre}-{browser}-hw{hw}-{t}')
        if r and r['page'].get('tiles'):
            x = list(r['page']['tiles'].values())[0]
            if x.get('impl'): return x['impl'] + (' (power-efficient)' if x.get('power_eff') else '')
    return 'n/a'

def cell(r, is_main):
    c = r['cpu']; p = r['page']; t = list(p['tiles'].values())
    rec = sum(x['received'] for x in t) or 1
    dr = sum(x['dropped'] for x in t); rd = sum(x.get('render_dropped', 0) for x in t); rt = sum(x.get('render_total', 0) for x in t) or 1
    fz = sum(x['freezes'] for x in t); fzs = sum(x['freeze_s'] for x in t)
    return (f"{c['cpu_mean']} / {c['cpu_max']} | {c['thread_mean']} / {c['thread_max']} | {g(r,'mine','videodecode')} | {g(r,'mine','3d')} | {c['ws_mb']:.0f} | "
            f"{min(x['fps'] for x in t)} | {dr} ({100*dr/rec:.2f}%) | {rd} ({100*rd/rt:.1f}%) | {fz} ({fzs:.1f} s) | {r['verdict']}")

HDR = "| browser | HW decode | CPU % of 1 core mean / max | busiest thread % mean / max | 1070 Ti VideoDecode % | 1070 Ti 3D % | working set MB | min fps | decoder-dropped frames (6 tiles) | render-dropped (6 tiles) | freezes (total s) | verdict |\n|---|---|---|---|---|---|---|---|---|---|---|---|"
print('## Spike B - six tiles, 3 min sampled after 30 s warm-up (CPU is the sum over ALL processes of the test browser; GPU is the test browser\'s own engine use)\n')
for tag, title, main in (('sub6', '(ii) six 640x360 sub-stream stand-ins', False), ('main6', '(i) six 2560x1440 mains', True), ('hd6', '(iii) six 1280x720', False), ('focus', '(iv) 5 x 640x360 + 1 x 2560x1440 focused (3x3, main spans 2x2)', True)):
    print(f'### {title}\n\n{HDR}')
    for b in ('chrome', 'edge'):
        for hw in ('on', 'off'):
            r = S.get(f'B-{tag}-{b}-hw{hw}')
            if r: print(f"| {b} | {hw} ({impl_of(b, hw, 'm1' if main else 's1')}) | {cell(r, main)} |")
    r = S.get(f'B-{tag}-mpv')
    if r:
        c = r['cpu']; m = r['mpv']
        dr = sum(x['vo_drops'] + x['dec_drops'] for x in m)
        print(f"| mpv (dashboard flags) | software, hwdec={m[0]['hwdec']} | {c['cpu_mean']} / {c['cpu_max']} | {c['thread_mean']} / {c['thread_max']} | {g(r,'mine','videodecode')} | {g(r,'mine','3d')} | {c['ws_mb']:.0f} | - | {dr} (VO+decoder) | - | - | {r['verdict']} |")
    print()
i = S.get('B-idle')
if i: print(f"Noise floor (the sampler process alone, no browser; rig = MediaMTX + publishers running): sampler CPU {i['cpu']['cpu_mean']}% of a core, rig CPU {i['cpu']['rig_cpu']}%, WHOLE-MACHINE CPU {i['cpu']['sys_cpu']}%.\n")
print('## Whole-machine CPU during each cell (%, 4 logical CPUs = 100)\n\n| cell | machine CPU % | rig CPU % of 1 core |\n|---|---|---|')
for k in sorted(S):
    if k.startswith('B-') and 'cpu' in S[k]: print(f"| {k[2:]} | {S[k]['cpu']['sys_cpu']} | {S[k]['cpu']['rig_cpu']} |")
print('\n## Hidden-tab runs (6 x 1440p, HW decode on): window minimized after 60 s\n\n| browser | phase | CPU % mean | 1070 Ti VideoDecode % | decoded fps per tile | page visibilityState |\n|---|---|---|---|---|---|')
for b in ('chrome', 'edge'):
    r = S.get(f'B-main6-{b}-hidden')
    if not r: continue
    for ph in ('visible', 'hidden'):
        x = r[ph]; t = list(x['page']['tiles'].values())
        print(f"| {b} | {ph} | {x['cpu']['cpu_mean']} | {x['gpu']['mine'].get('NVIDIA GeForce GTX 1070 Ti|videodecode', [0])[0]} | {min(y['fps'] for y in t)}-{max(y['fps'] for y in t)} | {','.join(x['page']['vis'])} |")
print('\n## Spike A - one 2560x1440 tile\n\n| run | browser | decoder implementation | CPU % mean / max | busiest thread % | 1070 Ti VideoDecode % | 1070 Ti 3D % | WS MB | fps | dropped / freezes | jitter-buffer ms | decode ms/frame | time to first frame ms (incl. ICE gather) | note |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|---|')
for k in sorted(S):
    if k.startswith('A') and 'clean' in k or (k.startswith('A') and 'probe' in k):
        r = S[k]; c = r['cpu']; p = r['page']; t = list(p['tiles'].values()) if p.get('tiles') else []
        if not t or not t[0].get('impl') and t[0]['fps'] == 0 and 'firefox' not in k: pass
        x = t[0] if t else {}
        note = []
        if 'probe' in k: note.append('canvas readback + unmute: CPU/GPU inflated by the probe')
        if 'firefox' in k: note.append('never connected (ICE failed)')
        if 'latency_s' in r: note.append(f"barcode latency median {r['latency_s']['median']} s (p10 {r['latency_s']['p10']}, p90 {r['latency_s']['p90']}); receive-to-callback median {r['recv_to_callback_ms']['median']} ms")
        if x.get('audio_energy_d'): note.append(f"audio energy +{x['audio_energy_d']}, level {x.get('audio_level_last')}")
        print(f"| {k} | {r['meta']['browser']} | {x.get('impl')} | {c['cpu_mean']} / {c['cpu_max']} | {c['thread_mean']} | {g(r,'mine','videodecode')} | {g(r,'mine','3d')} | {c['ws_mb']:.0f} | {x.get('fps')} | {x.get('dropped')} / {x.get('freezes')} | {x.get('jb_ms')} | {x.get('dec_ms')} | {x.get('ttff_ms')} | {'; '.join(note)} |")
print('\n## Decoder implementation runs (single tile, capture on so getStats exposes it)\n\n| run | implementation | power-efficient | 1070 Ti VideoDecode % | HD 530 VideoDecode % |\n|---|---|---|---|---|')
for k in sorted(S):
    if k.startswith('I2-'):
        r = S[k]; t = list(r['page']['tiles'].values()) if r['page'].get('tiles') else []
        x = t[0] if t else {}
        print(f"| {k} | {x.get('impl')} | {x.get('power_eff')} | {g(r,'mine','videodecode')} | {g(r,'mine','videodecode','530')} |")
