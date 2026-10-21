#!/usr/bin/env python3
"""P06.3 验收用的最小 WebSocket 客户端（只用标准库）。

VM 上没有浏览器，也没有 websocket-client / websockets 之类的第三方包；
P06.3 的验收又必须真的走一遍浏览器那一跳（Origin 校验、子协议里的许可、
文本帧、ping/pong、被服务端断开）。所以这里手写一个够用的客户端：

  - 握手：自己拼 GET 升级请求，可以精确控制有没有 Origin、带哪些子协议、
    带什么 Cookie —— 这几项正是握手那四道检查的输入，用现成库反而不好构造
    “故意错的请求”。
  - 只做验收需要的部分：文本帧、掩码、分片重组、pong 应答、close。
    不做压缩、不做二进制帧（终端协议就是文本帧，收到二进制说明服务端坏了，
    这里按“意外的帧”记下来）。
  - 步骤用 JSON 计划描述（sleep / input / resize / send / expect_output /
    expect_frame / drain / close / abort），因此“等到某个输出出现”“等到被
    断开”这类时序断言不需要在 shell 里写循环。
  - 结果写成一份 JSON 报告：状态码、协商到的子协议、收到的每一帧、累计的
    终端输出、每条断言的结论、字节数与结束原因。shell 只做“读字段 + 断言”，
    归因（是握手被拒还是连上后超时）看报告。

用法：
  python3 p06-3-ws-client.py --url ws://127.0.0.1:19463/api/v1/terminal/sessions/<id>/connect \\
      --origin http://127.0.0.1:19463 --cookie "upanel_session=..." \\
      --protocol upanel.term.v1 --plan plan.json --out report.json

退出码：0 报告已写出（结论在报告里）；2 参数或计划本身有问题。
"""

import argparse
import base64
import hashlib
import json
import os
import socket
import struct
import sys
import time
import traceback
from urllib.parse import urlsplit

WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
OP_CONT, OP_TEXT, OP_BIN, OP_CLOSE, OP_PING, OP_PONG = 0x0, 0x1, 0x2, 0x8, 0x9, 0xA

# 报告里保留的证据量：帧太多（比如速率限制那一轮会收到一帧 error 但之前可能
# 已经回显了几十帧）时截断，但计数不截断。
MAX_STORED_FRAMES = 300
MAX_STORED_OUTPUT = 200_000


class Fail(Exception):
    """本次连接在预期之外结束（EOF、超时、协议违规）。"""


class Buffered:
    """带缓冲的 socket 读取器：HTTP 头之后的剩余字节要交给帧解析器。"""

    def __init__(self, sock):
        self.sock = sock
        self.buf = b""

    def fill(self, deadline):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise Fail("timeout")
        self.sock.settimeout(remaining)
        try:
            chunk = self.sock.recv(65536)
        except socket.timeout:
            raise Fail("timeout")
        if not chunk:
            raise Fail("eof")
        self.buf += chunk

    def need(self, n, deadline):
        while len(self.buf) < n:
            self.fill(deadline)
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def read_until(self, marker, deadline):
        while marker not in self.buf:
            self.fill(deadline)
        head, self.buf = self.buf.split(marker, 1)
        return head

    def drain(self, want, deadline):
        """读到 EOF 或读满 want 字节；返回读到的全部内容。"""
        try:
            while len(self.buf) < want:
                self.fill(deadline)
        except Fail:
            pass
        out, self.buf = self.buf, b""
        return out


def parse_http_head(raw):
    lines = raw.decode("latin-1").split("\r\n")
    status_line = lines[0] if lines else ""
    parts = status_line.split(" ", 2)
    code = 0
    if len(parts) >= 2 and parts[1].isdigit():
        code = int(parts[1])
    headers = {}
    for line in lines[1:]:
        if ":" in line:
            k, v = line.split(":", 1)
            headers[k.strip().lower()] = v.strip()
    return code, headers, status_line


def read_body(b, headers, deadline):
    """按 Content-Length / chunked / 读到 EOF 三种方式读响应体。"""
    length = headers.get("content-length", "")
    if length.isdigit():
        return b.need(int(length), deadline).decode("utf-8", "replace")
    if "chunked" in headers.get("transfer-encoding", "").lower():
        out = b""
        while True:
            size_line = b.read_until(b"\r\n", deadline)
            try:
                size = int(size_line.split(b";")[0].strip() or b"0", 16)
            except ValueError:
                raise Fail("chunked 长度行无法解析")
            if size == 0:
                b.read_until(b"\r\n", deadline)  # 尾部空行
                return out.decode("utf-8", "replace")
            out += b.need(size, deadline)
            b.need(2, deadline)  # 块尾的 CRLF
    return b.drain(1 << 20, deadline).decode("utf-8", "replace")


def send_ws_frame(sock, opcode, payload):
    if isinstance(payload, str):
        payload = payload.encode("utf-8")
    mask = os.urandom(4)
    n = len(payload)
    first = 0x80 | opcode  # FIN=1，服务端不会给我们分片外的控制帧
    if n < 126:
        header = struct.pack("!BB", first, 0x80 | n)
    elif n < 65536:
        header = struct.pack("!BBH", first, 0x80 | 126, n)
    else:
        header = struct.pack("!BBQ", first, 0x80 | 127, n)
    # 客户端帧必须掩码（RFC 6455 §5.3）。用整数异或而不是逐字节 python 循环，
    # 70 KiB 的越界帧也只要几毫秒。
    keystream = (mask * ((n // 4) + 1))[:n]
    masked = (int.from_bytes(payload, "big") ^ int.from_bytes(keystream, "big")).to_bytes(n, "big") if n else b""
    sock.sendall(header + mask + masked)


def recv_ws_frame(b, deadline):
    b0, b1 = b.need(2, deadline)
    opcode = b0 & 0x0F
    n = b1 & 0x7F
    if n == 126:
        n = struct.unpack("!H", b.need(2, deadline))[0]
    elif n == 127:
        n = struct.unpack("!Q", b.need(8, deadline))[0]
    mask = b.need(4, deadline) if (b1 & 0x80) else None
    payload = b.need(n, deadline) if n else b""
    if mask and payload:
        payload = bytes(payload[i] ^ mask[i % 4] for i in range(len(payload)))
    return opcode, payload, bool(b0 & 0x80)


class Session:
    """一条已升级的终端连接 + 报告状态。"""

    def __init__(self, sock, report):
        self.sock = sock
        self.b = Buffered(sock)
        self.report = report
        self.output = bytearray()
        self.output_total = 0
        self.frames = []
        self.counts = {}
        self.exit = None
        self.errors = []
        self.closed = False
        self.close_code = None
        self.msg_opcode = None
        self.msg_buf = bytearray()
        self.consumed = 0  # expect_frame 已经看过的位置

    # ---- 帧输入 ----

    def _store(self, obj):
        self.counts[obj.get("type", "?")] = self.counts.get(obj.get("type", "?"), 0) + 1
        if len(self.frames) < MAX_STORED_FRAMES:
            self.frames.append(obj)

    def _handle_text(self, text):
        try:
            obj = json.loads(text)
            if not isinstance(obj, dict):
                obj = {"_raw": text[:400]}
        except Exception:
            obj = {"_raw": text[:400]}
        self._store(obj)
        kind = obj.get("type")
        if kind == "output":
            data = base64.b64decode(obj.get("data_b64") or "")
            self.output_total += len(data)
            room = MAX_STORED_OUTPUT - len(self.output)
            if room > 0:
                self.output += data[:room]
        elif kind == "exit":
            self.exit = obj
        elif kind == "error":
            self.errors.append(obj)

    def pump(self, deadline):
        """收一条消息；控制帧就地处理。连接结束抛 Fail。"""
        while True:
            opcode, payload, fin = recv_ws_frame(self.b, deadline)
            if opcode == OP_CLOSE:
                self.closed = True
                self.close_code = struct.unpack("!H", payload[:2])[0] if len(payload) >= 2 else None
                self.report["close_payload"] = payload[:120].decode("utf-8", "replace")
                raise Fail("closed")
            if opcode == OP_PING:
                self._send_quiet(OP_PONG, payload)
                continue
            if opcode == OP_PONG:
                continue
            if opcode == OP_BIN:
                self._store({"_binary": True, "size": len(payload)})
                continue
            if opcode in (OP_TEXT, OP_CONT):
                if opcode == OP_TEXT:
                    self.msg_opcode = OP_TEXT
                    self.msg_buf = bytearray(payload)
                else:
                    self.msg_buf += payload
                if not fin:
                    continue
                text = self.msg_buf.decode("utf-8", "replace")
                self.msg_buf = bytearray()
                self._handle_text(text)
                return

    def _send_quiet(self, opcode, payload):
        """发控制帧；连接已经断了就当作没发（pong 晚到一点没有意义）。"""
        try:
            send_ws_frame(self.sock, opcode, payload)
            return True
        except OSError:
            return False

    def pump_until(self, predicate, timeout):
        deadline = time.monotonic() + timeout
        while True:
            if predicate():
                return True
            if self.closed:
                return predicate()
            self.pump(deadline)

    def sleep(self, seconds):
        """睡一段时间，但**继续读**。

        终端这条链路上有两跳都带心跳（Web 每 20 秒 ping、60 秒收不到 pong 就
        判定对端已死）。真的 time.sleep(120) 会让一次“最长会话”的验收变成
        “心跳超时”的验收。所以 sleep 必须边睡边读，把 ping 应答了。
        """
        end = time.monotonic() + seconds
        while not self.closed:
            remaining = end - time.monotonic()
            if remaining <= 0:
                return
            try:
                self.pump(time.monotonic() + min(remaining, 0.5))
            except Fail:
                if self.closed:
                    return
                # 0.5 秒没收到东西不是错误，继续睡。


    def text(self):
        return self.output.decode("utf-8", "replace")

    # ---- 帧输出 ----

    def send_json(self, obj):
        send_ws_frame(self.sock, OP_TEXT, json.dumps(obj))

    def send_input(self, data):
        if isinstance(data, str):
            data = data.encode("utf-8")
        self.send_json({"type": "input", "data_b64": base64.b64encode(data).decode()})

    def send_resize(self, cols, rows):
        self.send_json({"type": "resize", "cols": cols, "rows": rows})

    def close(self, code=1000):
        try:
            send_ws_frame(self.sock, OP_CLOSE, struct.pack("!H", code))
        except OSError:
            pass
        try:
            self.sock.shutdown(socket.SHUT_WR)
        except OSError:
            pass
        try:
            self.sock.close()
        except OSError:
            pass
        self.closed = True


def handshake(url, origin, cookie, protocols, timeout):
    parts = urlsplit(url)
    host = parts.hostname
    port = parts.port or (443 if parts.scheme == "wss" else 80)
    path = parts.path or "/"
    if parts.query:
        path += "?" + parts.query

    sock = socket.create_connection((host, port), timeout=timeout)
    key = base64.b64encode(os.urandom(16)).decode()
    lines = [
        "GET %s HTTP/1.1" % path,
        "Host: %s:%d" % (host, port),
        "Upgrade: websocket",
        "Connection: Upgrade",
        "Sec-WebSocket-Key: %s" % key,
        "Sec-WebSocket-Version: 13",
        "User-Agent: upanel-acceptance-p06-3",
    ]
    if origin:
        lines.append("Origin: %s" % origin)
    if cookie:
        lines.append("Cookie: %s" % cookie)
    if protocols:
        lines.append("Sec-WebSocket-Protocol: %s" % ", ".join(protocols))
    sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode("utf-8"))

    b = Buffered(sock)
    deadline = time.monotonic() + timeout
    raw = b.read_until(b"\r\n\r\n", deadline)
    code, headers, status_line = parse_http_head(raw)
    return sock, b, key, code, headers, status_line


def verify_accept(key, headers):
    want = base64.b64encode(hashlib.sha1((key + WS_GUID).encode()).digest()).decode()
    got = headers.get("sec-websocket-accept", "")
    return want == got, want, got


def run_plan(sess, steps, report):
    for index, step in enumerate(steps):
        if not isinstance(step, dict):
            raise SystemExit("计划第 %d 步不是对象" % index)
        ended = False
        if "sleep" in step:
            sess.sleep(float(step["sleep"]))
        elif "input" in step:
            # 服务端可能已经先一步关掉了连接（最长会话那一类），这时发东西
            # 没有意义，但不该让整份计划失败：后面的 expect_* 仍要看已有帧。
            sess._send_quiet(OP_TEXT, json.dumps({
                "type": "input", "data_b64": base64.b64encode(
                    step["input"].encode() if isinstance(step["input"], str) else step["input"]).decode(),
            }))
        elif "resize" in step:
            cols, rows = step["resize"]
            sess._send_quiet(OP_TEXT, json.dumps({"type": "resize", "cols": int(cols), "rows": int(rows)}))
        elif "send" in step:
            sess._send_quiet(OP_TEXT, json.dumps(step["send"]))
        elif "send_raw" in step:
            sess._send_quiet(OP_TEXT, step["send_raw"])
        elif "burst" in step:
            b = step["burst"]
            count = int(b.get("count", 1))
            text = b.get("text", "x")
            payload = json.dumps({"type": "input", "data_b64": base64.b64encode(text.encode()).decode()})
            # 速率限制测的是“帧率”，因此不 sleep：一次 sendall 一串。
            blob = b""
            mask = os.urandom(4)
            raw = payload.encode()
            n = len(raw)
            keystream = (mask * ((n // 4) + 1))[:n]
            body = (int.from_bytes(raw, "big") ^ int.from_bytes(keystream, "big")).to_bytes(n, "big")
            head = struct.pack("!BBH", 0x81, 0x80 | 126, n)
            one = head + mask + body
            for _ in range(count):
                blob += one
            try:
                sess.sock.sendall(blob)
            except OSError:
                pass
            report["burst_sent"] = count
        elif "expect_output" in step:
            e = step["expect_output"]
            needle = e["contains"]
            timeout = float(e.get("timeout", 15))
            ok = False
            try:
                ok = sess.pump_until(lambda: needle in sess.text(), timeout)
            except Fail:
                ok = needle in sess.text()
            report["expectations"].append({
                "name": e.get("name", needle[:40]),
                "kind": "output",
                "needle": needle,
                "ok": ok,
            })
        elif "expect_frame" in step:
            e = step["expect_frame"]
            timeout = float(e.get("timeout", 10))
            want_type = e.get("type")
            want_code = e.get("code")

            def found():
                while sess.consumed < len(sess.frames):
                    f = sess.frames[sess.consumed]
                    sess.consumed += 1
                    if f.get("type") == want_type and (want_code is None or f.get("code") == want_code):
                        return True
                return False

            ok = False
            try:
                ok = sess.pump_until(found, timeout)
            except Fail:
                ok = found()
            report["expectations"].append({
                "name": e.get("name", "%s/%s" % (want_type, want_code)),
                "kind": "frame",
                "ok": ok,
            })
        elif "drain" in step:
            deadline = time.monotonic() + float(step["drain"])
            try:
                while True:
                    sess.pump(deadline)
            except Fail:
                pass
        elif "close" in step:
            sess.close(int(step["close"]) if isinstance(step["close"], int) else 1000)
            ended = True
        elif "abort" in step:
            # 不发 close 帧直接断 TCP：模拟浏览器崩溃/拔网线，走的是断流宽限那条路。
            try:
                sess.sock.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
                sess.sock.close()
            except OSError:
                pass
            sess.closed = True
            ended = True
        else:
            raise SystemExit("计划第 %d 步无法识别：%s" % (index, json.dumps(step, ensure_ascii=False)))
        if ended:
            break


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--origin", default="")
    ap.add_argument("--cookie", default="")
    ap.add_argument("--protocol", action="append", default=[])
    ap.add_argument("--plan", default="")
    ap.add_argument("--out", required=True)
    ap.add_argument("--timeout", type=float, default=20.0)
    ap.add_argument("--tail", type=float, default=0.5)
    # --tail 是计划跑完之后再读一小段的时间：服务端在收到越界帧/超速率之后是
    # “先回 error 帧再断开”，不给它这半秒，报告里就没有那次关闭的码与原因，
    # 断言也就只能看到 error 帧而看不到“随后被断开”。
    args = ap.parse_args()

    # --origin none 表示“故意不发 Origin 头”。空串也一样，写清楚是为了让
    # 调用方在断言里一眼看出这不是“忘了传”。
    origin = "" if args.origin in ("", "none") else args.origin

    protocols = []
    for p in args.protocol:
        protocols.extend([x for x in p.split(",") if x])

    report = {
        "url": args.url,
        "origin": origin or None,
        "protocols": protocols,
        "status": None,
        "status_line": "",
        "headers": {},
        "subprotocol": None,
        "accept_ok": None,
        "body": "",
        "frames": [],
        "frame_counts": {},
        "output": "",
        "output_bytes": 0,
        "exit": None,
        "errors": [],
        "expectations": [],
        "close_code": None,
        "duration_ms": 0,
        "error": None,
    }
    started = time.monotonic()
    sock = None
    try:
        sock, buf, key, code, headers, status_line = handshake(
            args.url, origin, args.cookie, protocols, args.timeout)
        report["status"] = code
        report["status_line"] = status_line
        report["headers"] = headers
        if code != 101:
            report["body"] = read_body(buf, headers, time.monotonic() + args.timeout)
        else:
            ok, want, got = verify_accept(key, headers)
            report["accept_ok"] = ok
            report["accept_want"] = want
            report["accept_got"] = got
            report["subprotocol"] = headers.get("sec-websocket-protocol")
            sess = Session(sock, report)
            steps = []
            if args.plan:
                with open(args.plan, encoding="utf-8") as fh:
                    plan = json.load(fh)
                steps = plan["steps"] if isinstance(plan, dict) else plan
            try:
                run_plan(sess, steps, report)
                if not sess.closed and args.tail > 0:
                    deadline = time.monotonic() + args.tail
                    try:
                        while True:
                            sess.pump(deadline)
                    except Fail:
                        pass
            finally:
                report["frames"] = sess.frames
                report["frame_counts"] = sess.counts
                report["output"] = sess.output.decode("utf-8", "replace")
                report["output_bytes"] = sess.output_total
                report["exit"] = sess.exit
                report["errors"] = sess.errors
                report["close_code"] = sess.close_code
                report["closed_by_server"] = sess.closed
                if not sess.closed:
                    sess.close()
    except Fail as exc:
        report["error"] = "fail: %s" % exc
    except Exception as exc:  # 网络、DNS、计划文件错误都要进报告，不能只剩 traceback
        report["error"] = "%s: %s" % (type(exc).__name__, exc)
        report["traceback"] = traceback.format_exc(limit=4)
    finally:
        report["duration_ms"] = int((time.monotonic() - started) * 1000)
        if sock is not None:
            try:
                sock.close()
            except OSError:
                pass
        with open(args.out, "w", encoding="utf-8") as fh:
            json.dump(report, fh, ensure_ascii=False, indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())
