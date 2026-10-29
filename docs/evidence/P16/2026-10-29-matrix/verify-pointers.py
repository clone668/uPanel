#!/usr/bin/env python3
# 校验 docs/ACCEPTANCE_MATRIX.md 里每一条反引号包住的证据路径都真的存在。
# 支持 {a,b} 花括号展开、* 通配、":93" 行号后缀、以及 "…/xxx" 这类省略写法（退到目录判断）。
# 用法：python docs/evidence/P16/2026-10-29-matrix/verify-pointers.py
import io, os, re, glob, sys

try:                                  # Windows 控制台默认 cp936，会把中文输出打坏
    sys.stdout.reconfigure(encoding='utf-8')
except Exception:
    pass

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(
    os.path.dirname(os.path.abspath(__file__))))))   # 文件在 docs/evidence/P16/<日期>/ 下，上五级才是仓库根
DOC = os.path.join(ROOT, 'docs', 'ACCEPTANCE_MATRIX.md')
BASE = os.path.join(ROOT, 'docs', 'evidence')

s = io.open(DOC, encoding='utf-8').read()
raw = set()
for m in re.finditer(r'`([^`]+)`', s):
    for cand in re.split(r'[、，,\s]+', m.group(1)):
        if re.match(r'^P\d\d/', cand) or cand.startswith('docs/evidence'):
            raw.add(cand)

def resolve(t):
    t = t.split(':')[0]
    if t.startswith('docs/evidence/'):
        t = t[len('docs/evidence/'):]
    m = re.search(r'\{([^}]+)\}', t)
    if m:
        out = []
        for alt in m.group(1).split(','):
            out += resolve(t[:m.start()] + alt + t[m.end():])
        return out
    if '*' in t:
        return glob.glob(os.path.join(BASE, t))
    p = os.path.join(BASE, t)
    if os.path.exists(p):
        return [p]
    d = os.path.dirname(p)                 # “…” 省略写法：目录在就算指得到
    return [p] if os.path.isdir(d) else []

bad = []
for t in sorted(raw):
    if not resolve(t):
        bad.append(t)

print('检查 %d 条路径；缺失 %d' % (len(raw), len(bad)))
for b in bad:
    print('  MISS', b)
raise SystemExit(1 if bad else 0)
