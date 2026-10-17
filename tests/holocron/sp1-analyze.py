# SP1: per case, list segment names, mkv duration (ffprobe) and name-time alignment.
# usage: python3 -I sp1-analyze.py <root> <case>...    (root holds <case>/ folders and <case>.exit/.stderr/.stdout)
import sys, os, re, subprocess, calendar
root = sys.argv[1]
for case in sys.argv[2:]:
    seg = int(case.rsplit('_s', 1)[1])
    d = os.path.join(root, case)
    ex = open(os.path.join(root, case + '.exit')).read().strip() if os.path.exists(os.path.join(root, case + '.exit')) else '?'
    so = os.path.getsize(os.path.join(root, case + '.stdout')) if os.path.exists(os.path.join(root, case + '.stdout')) else -1
    print(f'== {case} exit={ex} stdout_bytes={so}')
    for f in sorted(os.listdir(d)):
        m = re.match(r'Test_(\d{4})(\d\d)(\d\d)_(\d\d)(\d\d)(\d\d)([+-])(\d\d)(\d\d)\.mkv$', f)
        if not m:
            print('  ?? unexpected name', f); continue
        y, mo, da, h, mi, s = map(int, m.groups()[:6]); sign = 1 if m.group(7) == '+' else -1
        off = sign * (int(m.group(8)) * 3600 + int(m.group(9)) * 60)
        local_sod = h * 3600 + mi * 60 + s
        epoch = calendar.timegm((y, mo, da, h, mi, s)) - off
        r = subprocess.run(['ffprobe', '-v', 'error', '-show_entries', 'format=duration', '-of', 'csv=p=0', os.path.join(d, f)], capture_output=True, text=True)
        dur = r.stdout.strip() or 'ERR'
        print(f'  {f[5:]}  dur={dur:>9}  local%{seg}={local_sod % seg}  utc%{seg}={(epoch % 86400) % seg}  epoch%{seg}={epoch % seg}')
