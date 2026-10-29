import sys

sys.stdout.reconfigure(encoding='utf-8')


def rep1(path, old, new, tag):
    s = open(path, encoding='utf-8').read()
    n = s.count(old)
    assert n == 1, '%s: 命中 %d 次' % (tag, n)
    open(path, 'w', encoding='utf-8', newline='\n').write(s.replace(old, new))
    print('OK', tag)


P = 'docs/PROGRESS.md'
rep1(P, '**核心验收矩阵已逐项对照**：STAGES §19 的 31 条**已验 27 / 部分 4 / 未验 0**，见 `docs/ACCEPTANCE_MATRIX.md`',
     '**核心验收矩阵已逐项对照**：STAGES §19 的 31 条**已验 28 / 部分 3 / 未验 0**，见 `docs/ACCEPTANCE_MATRIX.md`'
     '（PROXY-02 于 10-30 复核为引错证据，从"部分"改成"已验"）', 'progress §3 stage cell')

rep1(P, '* 总表之后是四条"部分"的展开：缺哪个子句、账本哪一行自己已经承认、要造什么现场才能补。',
     '* 总表之后是三条"部分"的展开：缺哪个子句、账本哪一行自己已经承认、要造什么现场才能补；\n'
     '  外加 §2.4 一条**更正**（PROXY-02 起初被判成部分，复核是引错证据）。', 'progress §5 deliverable 1')

rep1(P, '* 末节写清"证明了什么、没证明什么"：不证明 31 条就是全部风险；四条"部分"不能加总成已验；',
     '* 末节写清"证明了什么、没证明什么"：不证明 31 条就是全部风险；三条"部分"不能加总成已验；',
     'progress §5 deliverable 2')

rep1(P, '上面四条"部分"的缺口原样保留，一条都没有因为写进表就变成已验。',
     '上面三条"部分"的缺口原样保留，一条都没有因为写进表就变成已验。'
     '（PROXY-02 不在这三条里：它是 10-30 复核出的**引错证据**，改的是判定，不是读数。）',
     'progress §5 未验证')

R = 'docs/evidence/P16/2026-10-29-matrix/README.md'
rep1(R, '四条"部分"逐条展开缺口；RELEASE-01 单独按 P16 七相位展开',
     '三条"部分"逐条展开缺口 + 一条更正（PROXY-02）；RELEASE-01 单独按 P16 七相位展开',
     'readme deliverables row')

rep1(R, '它是**索引与定档**工作。四条"部分"的缺口原样保留，一条都没有因为写进表就变成已验。',
     '它是**索引与定档**工作。三条"部分"的缺口原样保留，一条都没有因为写进表就变成已验。'
     'PROXY-02 那次改判不是新读数换来的，是把 P10.4 早就跑过的读数补引进来。', 'readme §4')
