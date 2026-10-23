#!/usr/bin/env python3
"""P10.4 验收用的 WebSocket 客户端：走 nginx，读服务端推流（只用标准库）。

要证明的是三件事，缺一不可：

  1. **Upgrade 穿过了 nginx**：101 状态码 + 合法的 Sec-WebSocket-Accept。
     `Sec-WebSocket-Accept` 是用客户端随机 key 算出来的，nginx 转发不改它，
     所以 `accept_ok` 为真说明这一跳是真的被代理到了上游，而不是 nginx 自己
     编了个 200。
  2. **不断流**：上游按 0.5 秒一条推 12 条，客户端把它们**全部**收到，并且相邻
     两帧的间隔没有异常拉长（一次被当普通 HTTP 收尾，客户端只会拿到提前 EOF，
     `ticks_seen` 就对不上了；一次被中间层缓冲，`max_gap` 就会炸开）。
     因此判据是 ticks_seen == ticks 且 max_gap_ms < 3×间隔。
  3. **双向可用**：客户端发一条文本帧，收到 `echo:` 开头的回应。
     （上行被缓冲住的话，只会看到那条回应迟到或不到。）

既支持 ws:// 也支持 wss://：给 --ca 就用它当信任根（staging 证书由 Pebble 的根
签出来，系统信任库里也装了同一张，两种都行）；握手用 server_hostname=域名，
**不做**跳过校验——跳过校验就等于没验证“域名正常访问”。

报告写成 JSON（状态码、协商结果、每一帧的到达时刻、最大间隔、echo 结果、
对端证书的 sha256 指纹），shell 只读字段做断言，归因看报告。

用法：
  python3 p10-4-ws-through-proxy.py --url wss://p104.example.test/ws \\
      --ca /tmp/pebble-root.pem --ticks 12 --out ws.json

退出码：0 报告已写出（结论在报告里）；2 参数问题或连接根本没建立。
"""

import argparse
import base64
import hashlib
import json
import os
import socket
import ssl
import struct
import sys
import time
import traceback
from urllib.parse import urlsplit

WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
OP_CONT, OP_TEXT, OP_BIN, OP_CLOSE, OP_PING, OP_PONG = 0x0, 0x1, 0x2, 0x8, 0x9, 0xA


class Buffered:
    def __init__(self, sock):
        self.sock = sock
        self.buf = b""

    def need(self, n, deadline):
        while len(self.buf) < n:
            self.fill(deadline)
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def fill(self, deadline):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("timeout")
        self.sock.settimeout(remaining)
        chunk = self.sock.recv(65536)
        if not chunk:
            raise EOFError("eof")
        self.buf += chunk

    def read_until(self, marker, deadline):
        while marker not in self.buf:
            self.fill(deadline)
        head, self.buf = self.buf.split(marker, 1)
        return head


def send_frame(sock, opcode, payload):
    if isinstance(payload, str):
        payload = payload.encode("utf-8")
    mask = os.urandom(4)
    n = len(payload)
    header = bytearray([0x80 | opcode])
    if n < 126:
        header.append(0x80 | n)
    elif n < 65536:
        header.append(0x80 | 126)
        header += struct.pack("!H", n)
    else:
        header.append(0x80 | 127)
        header += struct.pack("!Q", n)
    keystream = (mask * ((n // 4) + 1))[:n]
    masked = bytes(payload[i] ^ keystream[i] for i in range(n))
    sock.sendall(bytes(header) + mask + masked)


def read_frame(b, deadline):
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


def parse_head(raw):
    lines = raw.decode("latin-1").split("\r\n")
    status_line = lines[0] if lines else ""
    parts = status_line.split(" ", 2)
    code = int(parts[1]) if len(parts) >= 2 and parts[1].isdigit() else 0
    headers = {}
    for line in lines[1:]:
        if ":" in line:
            k, v = line.split(":", 1)
            headers[k.strip().lower()] = v.strip()
    return code, headers, status_line


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", required=True)
    ap.add_argument("--ca", default="")
    ap.add_argument("--host-header", default="")
    ap.add_argument("--ticks", type=int, default=12)
    ap.add_argument("--send", default="p104-ping")
    ap.add_argument("--out", required=True)
    ap.add_argument("--timeout", type=float, default=20.0)
    ap.add_argument("--idle", type=float, default=4.0)
    args = ap.parse_args()

    parts = urlsplit(args.url)
    secure = parts.scheme == "wss"
    host = parts.hostname or ""
    port = parts.port or (443 if secure else 80)
    path = parts.path or "/"
    if parts.query:
        path += "?" + parts.query
    host_header = args.host_header or ("%s:%d" % (host, port) if parts.port else host)

    report = {
        "url": args.url,
        "status": None,
        "status_line": "",
        "headers": {},
        "accept_ok": None,
        "tls": {},
        "frames": [],
        "ticks_seen": 0,
        "echo": None,
        "max_gap_ms": None,
        "min_gap_ms": None,
        "duration_ms": 0,
        "closed_by_server": False,
        "close_code": None,
        "error": None,
    }
    started = time.monotonic()
    sock = None
    try:
        sock = socket.create_connection((host, port), timeout=args.timeout)
        if secure:
            ctx = ssl.create_default_context(cafile=args.ca or None)
            ctx.minimum_version = ssl.TLSVersion.TLSv1_2
            sock = ctx.wrap_socket(sock, server_hostname=host)
            cert = sock.getpeercert(binary_form=True) or b""
            report["tls"] = {
                "version": sock.version(),
                "cipher": (sock.cipher() or [None])[0],
                "peer_fingerprint_sha256": hashlib.sha256(cert).hexdigest() if cert else None,
                "subject": str(dict(x[0] for x in (sock.getpeercert() or {}).get("subject", []))),
            }

        key = base64.b64encode(os.urandom(16)).decode()
        lines = [
            "GET %s HTTP/1.1" % path,
            "Host: %s" % host_header,
            "Upgrade: websocket",
            "Connection: Upgrade",
            "Sec-WebSocket-Key: %s" % key,
            "Sec-WebSocket-Version: 13",
            "User-Agent: upanel-acceptance-p10-4",
        ]
        sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode("utf-8"))

        b = Buffered(sock)
        deadline = time.monotonic() + args.timeout
        code, headers, status_line = parse_head(b.read_until(b"\r\n\r\n", deadline))
        report["status"] = code
        report["status_line"] = status_line
        report["headers"] = headers
        if code != 101:
            report["error"] = "handshake: %s" % status_line
            raise SystemExit(0)

        want = base64.b64encode(hashlib.sha1((key + WS_GUID).encode()).digest()).decode()
        got = headers.get("sec-websocket-accept", "")
        report["accept_ok"] = want == got
        report["accept_want"] = want
        report["accept_got"] = got

        send_frame(sock, OP_TEXT, args.send)
        send_at = time.monotonic()
        last_text_at = None
        echo_deadline = time.monotonic() + args.timeout

        while True:
            # idle 判的是“多久没有新帧就算断流”，不是“整段会话最多读这么久”。
            # 必须在每读完一帧之后把 deadline 续到 now+idle：写成一个固定的绝对
            # deadline 时，12 条 × 0.5s 的推流会在第 8 条之前被自己的 deadline 掐住，
            # 报告成 ticks_seen=7 / error=timeout —— 把“节拍正常”误判成“断流”。
            try:
                opcode, payload, _fin = read_frame(b, time.monotonic() + args.idle)
            except (TimeoutError, EOFError) as exc:
                report["error"] = None if report["ticks_seen"] >= args.ticks else "%s" % exc
                break
            now = time.monotonic()
            if opcode == OP_CLOSE:
                report["closed_by_server"] = True
                report["close_code"] = struct.unpack("!H", payload[:2])[0] if len(payload) >= 2 else None
                break
            if opcode == OP_PING:
                send_frame(sock, OP_PONG, payload)
                continue
            if opcode == OP_PONG:
                continue
            if opcode in (OP_TEXT, OP_CONT):
                text = payload.decode("utf-8", "replace")
                if text.startswith("echo:"):
                    report["echo"] = text
                    report["echo_ms"] = int((now - send_at) * 1000)
                else:
                    if len(report["frames"]) < 200:
                        report["frames"].append({
                            "t_ms": int((now - started) * 1000),
                            "text": text[:200],
                        })
                    report["ticks_seen"] += 1
                    if last_text_at is not None:
                        gap = (now - last_text_at) * 1000
                        report["max_gap_ms"] = int(gap if report["max_gap_ms"] is None
                                                   else max(report["max_gap_ms"], gap))
                        report["min_gap_ms"] = int(gap if report["min_gap_ms"] is None
                                                   else min(report["min_gap_ms"], gap))
                    last_text_at = now
                    # 收齐了推流再等一小段拿 echo 与（可能的）服务端关闭。
                    if report["ticks_seen"] >= args.ticks and report["echo"] is not None:
                        break
                if now > echo_deadline:
                    break

        if report["error"] is None and report["ticks_seen"] < args.ticks:
            report["error"] = "incomplete: %d/%d" % (report["ticks_seen"], args.ticks)
    except SystemExit:
        pass
    except Exception as exc:
        report["error"] = "%s: %s" % (type(exc).__name__, exc)
        report["traceback"] = traceback.format_exc(limit=4)
    finally:
        report["duration_ms"] = int((time.monotonic() - started) * 1000)
        if sock is not None:
            try:
                send_frame(sock, OP_CLOSE, struct.pack("!H", 1000))
            except OSError:
                pass
            try:
                sock.close()
            except OSError:
                pass
        with open(args.out, "w", encoding="utf-8") as fh:
            json.dump(report, fh, ensure_ascii=False, indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())
