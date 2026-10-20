# 验收证据

每个阶段的验收证据放在：

    docs/evidence/<阶段编号>/<YYYY-MM-DD>/

例如 `docs/evidence/P07/2026-10-25/`。

## 每个目录至少包含

| 文件 | 内容 |
|---|---|
| `env.txt` | 系统与工具链版本：`lsb_release -a`、`uname -a`、`go version`、`node -v`、`npm -v`，以及 VM 磁盘 `lsblk -o NAME,SIZE,SERIAL` |
| `commands.log` | 实际执行的命令、退出码、必要的脱敏输出 |
| `result.md` | 验收编号（如 DISK-03）、预期、实际、结论（pass / fail / NOT RUN）、失败时的真实系统状态 |
| 可选截图 | 仅证明界面展示；不能替代系统状态证据 |

## 硬性规则

1. 证据必须来自**实际执行**。未执行的写 `NOT RUN` 并说明补做方式，不写“理论上通过”。
2. 区分证据类型：unit / mock integration / Linux VM integration / browser E2E / 人工观察。
   在 `result.md` 顶部标明本次证据属于哪一类。
3. 不提交：磁盘镜像、VM 内存转储、私钥、密码、Agent 密钥、真实用户数据、journal 全量导出。
4. 输出需要脱敏时，保留结构、替换敏感值，并在文件头注明“已脱敏”。
5. 大输出截断时保留首尾并注明截断位置与原始大小。

## 当前内容

- `P00/2026-10-20/`：开发宿主上的工具链核实与统一检查脚本运行记录。
  该目录**不包含任何 Linux 验收**；VM 相关项为 NOT RUN。
