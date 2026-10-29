#!/usr/bin/env python3
"""验收脚本的常驻 JSON 助手。

为什么要有它
------------
验收要在一台 QEMU **TCG 纯软件模拟**的 Ubuntu 24.04 上跑（无 KVM，2 vCPU）。
那台机器上**一次 `python3` 启动要 8.6—17.7 秒**（负载 8.9 时实测，`jq` 只要 0.3 秒）。
验收脚本每轮要问几百次 JSON，每次都付一次解释器启动开销——光这一项：

* 把一整轮验收拖慢一个数量级；
* 更要紧的是**把 120 秒的预检窗口吃掉**：真机实测「预检 21.9 秒 + 再认证 57.9 秒
  + 脚本自身 45 秒」越界，`POST /api/v1/jobs` 因此 409（预检在再认证完成的那一秒失效）。

所以这里起一个常驻进程，表达式进、结果出，一问一答。

**表达式语义与原来那条 `python3 -c 'eval(...)'` 完全一致**（同一个 `d`、同一个 `json`
模块、同样的错误串 `ERR:<类型>:<消息>`）：改的是"谁来解释这段表达式"，不是
"这段表达式断言什么"。断言本身一个字没动。

协议（stdin 每行一条请求，制表符分隔）::

    <seq>\\tget\\t<json 文件>\\t<python 表达式>
    <seq>\\tdumps\\t<json 文本>

回复（stdout 每行一条）::

    <seq>\\t<结果字符串>

结果里不含裸换行：`json.dumps` 会把换行转义成 `\\n`，其余模式返回的也都是单行文本。
"""

import json
import sys


def evaluate(line):
    """处理一条请求，返回该回给调用方的那一行。"""
    parts = line.split("\t", 3)
    seq = parts[0]
    mode = parts[1] if len(parts) > 1 else ""
    try:
        if mode == "get":
            path = parts[2]
            expr = parts[3]
            with open(path, encoding="utf-8") as fh:
                d = json.load(fh)
            # 表达式是验收脚本自己的常量，不是外部输入；这里要的就是 eval 的语义。
            value = eval(expr, {"d": d, "json": json})  # noqa: S307
        elif mode == "dumps":
            value = json.dumps(json.loads(parts[2]), ensure_ascii=False)
        else:
            value = "ERR:unknown-mode:%s" % mode
    except Exception as exc:  # 与原来那条 python3 -c 一样：出错也要回一句话
        value = "ERR:%s:%s" % (type(exc).__name__, exc)
    return "%s\t%s" % (seq, value)


def main():
    for line in sys.stdin:
        line = line.rstrip("\n")
        if not line:
            continue
        sys.stdout.write(evaluate(line) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
