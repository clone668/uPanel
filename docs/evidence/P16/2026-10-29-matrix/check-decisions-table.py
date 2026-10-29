# 分两件事量 `docs/DECISIONS.md` 第 8 节 Q 表：
#   ① 渲染出来的格数（GFM：`\|` 是字面竖线，不算分隔符）
#   ② 行内代码里没转义的竖线（真正会把单元格劈开的东西）
# 用法：python docs/evidence/P16/2026-10-29-matrix/check-decisions-table.py [输出文件]
import sys

sys.stdout.reconfigure(encoding='utf-8')

BS = chr(92)
PATH = 'docs/DECISIONS.md'
out = []


def emit(s=''):
    print(s)
    out.append(s)


lines = open(PATH, encoding='utf-8').read().split('\n')


def split_cells(line):
    cells, cur, i = [], '', 0
    while i < len(line):
        ch = line[i]
        if ch == BS and i + 1 < len(line) and line[i + 1] == '|':
            cur += '|'
            i += 2
            continue
        if ch == '|':
            cells.append(cur)
            cur = ''
            i += 1
            continue
        cur += ch
        i += 1
    cells.append(cur)
    if cells and cells[0].strip() == '':
        cells = cells[1:]
    if cells and cells[-1].strip() == '':
        cells = cells[:-1]
    return cells


def bad_pipes(line):
    """行内代码段里、且没被反斜杠转义的竖线个数。"""
    n, i, in_span = 0, 0, False
    while i < len(line):
        ch = line[i]
        if ch == BS and i + 1 < len(line):
            i += 2
            continue
        if ch == '`':
            in_span = not in_span
        elif ch == '|' and in_span:
            n += 1
        i += 1
    return n


from collections import Counter

qrows = [(i, l) for i, l in enumerate(lines, 1) if l.startswith('| Q')]
dist = Counter()
corrupt = []
other_not5 = []
for i, l in qrows:
    cells = split_cells(l)
    dist[len(cells)] += 1
    b = bad_pipes(l)
    if b:
        corrupt.append((i, cells[0], b, len(cells)))
    elif len(cells) != 5:
        other_not5.append((i, cells[0], len(cells)))

emit('文件 = docs/DECISIONS.md 第 8 节 Q 表')
emit('Q 行合计 = %d' % len(qrows))
emit('渲染格数分布 = %s' % dict(sorted(dist.items())))
emit('表头声明的列数 = 5（第 132 行：编号 / 日期 / 类型 / 内容 / 状态·处理）')
emit('渲染成 5 格（与表头一致）= %d' % dist[5])
emit('渲染不是 5 格 = %d' % (len(qrows) - dist[5]))
emit('')
emit('① 行内代码里没转义竖线的行 = %d，竖线共 %d 处' % (len(corrupt), sum(b for _, _, b, _ in corrupt)))
emit('   ' + '  '.join('%s(%d 处→%d 格)' % (q, b, k) for _, q, b, k in corrupt))
emit('')
deliberate = [x for x in other_not5 if x[2] == 6]
other = [x for x in other_not5 if x[2] != 6]
emit('② 代码干净、但仍渲染成 6 格（有意写成 6 字段，表头没跟着改）= %d 条' % len(deliberate))
emit('   %s ... %s' % (deliberate[0][1], deliberate[-1][1]) if deliberate else '   （无）')
emit('   编号：' + ' '.join(q for _, q, _ in deliberate))
emit('③ 其余不再 5 格的行（少格）= %d 条：%s' % (
    len(other), '  '.join('%s(第 %d 行, %d 格)' % (q, i, k) for i, q, k in other)))
emit('')
emit('小结：%d 条 Q 行里 %d 条渲染成 5 格；%d 条因代码段竖线被劈开；%d 条是有意的 6 字段；%d 条少格。'
     % (len(qrows), dist[5], len(corrupt), len(deliberate), len(other)))

if len(sys.argv) > 1:
    open(sys.argv[1], 'w', encoding='utf-8', newline='\n').write('\n'.join(out) + '\n')
