#!/usr/bin/env python3
"""P10.4 验收用的“被测应用”（只用标准库）。

P10 的验收里有一句必须当真的话：**域名正常访问指定应用，WebSocket 不断流**。
VM 上没有 Docker、没有 Node、也没有第三方包，所以这里写一个最小的上游：

  1. 普通 GET（不带 Upgrade）→ 200 + 一段带 marker 的 HTML。
     这一条用来证明“域名穿过 nginx 打到的是这个应用”，而不是 nginx 的欢迎页。
  2. 带 Upgrade: websocket 的 GET → 101，然后**服务端主动推流**：
     每 --interval 秒发一条 `<marker>:<n>` 文本帧，共 --ticks 条；
     期间把客户端发来的文本帧回成 `echo:<原文>`，收到 ping 回 pong。

为什么要服务端推流：一次握手成功、回一帧就结束的 WebSocket 证明不了“不断流”。
推流把时间拉长到 ticks×interval（默认 6 秒），中间任何一跳把连接当普通 HTTP
请求收尾，客户端就会提前 EOF —— 断言才有意义。

为什么每条连接一个线程：普通 GET 与 WebSocket 会话必须能并发。串行 accept 的话，
“先访问页面再连 WebSocket”这两个断言会互相排队，跑出来像“连不上”。

用法：
  python3 p10-4-ws-upstream.py --port 18091 --marker p104-marker --log upstream.log

环境变量/参数：--host（默认 127.0.0.1）、--ticks、--interval、--log。
退出码：0 正常退出；2 端口绑不上（说明上一轮的上游还没退）。
"""

import argparse
import base64
import hashlib
import socket
import sys
import threading
import time

WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
OP_CONT, OP_TEXT, OP_BIN, OP_CLOSE, OP_PING, OP_PONG = 0x0, 0x1, 0x2, 0x8, 0x9, 0xA

PRINT_LOCK = threading.Lock()


def log(msg, stream):
    with PRINT_LOCK:
        stream.write("%s %s\n" % (time.strftime("%H:%M:%S"), msg))
        stream.flush()


def recv_exact(conn, n):
    buf = b""
    while len(buf) < n:
        chunk = conn.recv(n - len(buf))
        if not chunk:
            return None
        buf += chunk
    return buf


def read_frame(conn):
    head = recv_exact(conn, 2)
    if head is None:
        return None
    b0, b1 = head[0], head[1]
    opcode = b0 & 0x0F
    n = b1 & 0x7F
    if n == 126:
        ext = recv_exact(conn, 2)
        if ext is None:
            return None
        n = int.from_bytes(ext, "big")
    elif n == 127:
        ext = recv_exact(conn, 8)
        if ext is None:
            return None
        n = int.from_bytes(ext, "big")
    mask = recv_exact(conn, 4) if (b1 & 0x80) else None
    payload = recv_exact(conn, n) if n else b""
    if payload is None:
        return None
    if mask and payload:
        payload = bytes(payload[i] ^ mask[i % 4] for i in range(len(payload)))
    return opcode, payload, bool(b0 & 0x80)


def send_frame(conn, opcode, payload=b""):
    if isinstance(payload, str):
        payload = payload.encode("utf-8")
    n = len(payload)
    header = bytearray([0x80 | opcode])
    if n < 126:
        header.append(n)
    elif n < 65536:
        header.append(126)
        header += n.to_bytes(2, "big")
    else:
        header.append(127)
        header += n.to_bytes(8, "big")
    conn.sendall(bytes(header) + payload)


class Session:
    """一条已升级的连接：读线程应答控制帧与 echo，主线程按节奏推流。"""

    def __init__(self, conn, marker, ticks, interval, logf):
        self.conn = conn
        self.marker = marker
        self.ticks = ticks
        self.interval = interval
        self.logf = logf
        self.stop = threading.Event()
        self.sent = 0
        self.echoed = 0

    def reader(self):
        while not self.stop.is_set():
            try:
                frame = read_frame(self.conn)
            except OSError:
                break
            if frame is None:
                break
            opcode, payload, _fin = frame
            if opcode == OP_CLOSE:
                log("客户端发来 close，回复并结束会话", self.logf)
                try:
                    send_frame(self.conn, OP_CLOSE, payload[:2])
                except OSError:
                    pass
                break
            if opcode == OP_PING:
                try:
                    send_frame(self.conn, OP_PONG, payload)
                except OSError:
                    break
                continue
            if opcode in (OP_TEXT, OP_CONT):
                text = payload.decode("utf-8", "replace")
                self.echoed += 1
                try:
                    send_frame(self.conn, OP_TEXT, "echo:%s" % text)
                except OSError:
                    break
        self.stop.set()

    def run(self):
        threading.Thread(target=self.reader, daemon=True).start()
        for i in range(1, self.ticks + 1):
            if self.stop.wait(self.interval):
                break
            try:
                send_frame(self.conn, OP_TEXT, "%s:%d" % (self.marker, i))
                self.sent += 1
            except OSError:
                break
        # 推完之后**不主动关**：客户端要自己决定什么时候走。服务端先关的话，
        # “12 条都收到了”与“连接被对端掐断”在客户端报告里会混成同一件事。
        while not self.stop.wait(0.2):
            pass
        log("会话结束：sent=%d echoed=%d" % (self.sent, self.echoed), self.logf)
        try:
            self.conn.close()
        except OSError:
            pass


def parse_head(conn):
    data = b""
    while b"\r\n\r\n" not in data:
        chunk = conn.recv(4096)
        if not chunk:
            return None
        data += chunk
        if len(data) > 65536:
            return None
    text = data.decode("iso-8859-1")
    head = text.split("\r\n\r\n", 1)[0]
    lines = head.split("\r\n")
    headers = {}
    for line in lines[1:]:
        if ":" in line:
            k, v = line.split(":", 1)
            headers[k.strip().lower()] = v.strip()
    return lines[0] if lines else "", headers


def serve(conn, addr, args, logf):
    try:
        parsed = parse_head(conn)
        if parsed is None:
            return
        request_line, headers = parsed
        upgrade = headers.get("upgrade", "").lower()
        key = headers.get("sec-websocket-key", "")
        log("请求 %s upgrade=%r host=%r" % (request_line, upgrade, headers.get("host", "")), logf)

        if "websocket" not in upgrade or not key:
            body = ("<!doctype html><meta charset=utf-8><title>%s</title>"
                    "<h1>%s</h1><p>P10.4 upstream, host=%s</p>"
                    % (args.marker, args.marker, headers.get("host", ""))).encode("utf-8")
            conn.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\n"
                         + b"Content-Length: " + str(len(body)).encode() + b"\r\n"
                         + b"Connection: close\r\n\r\n" + body)
            return

        accept = base64.b64encode(
            hashlib.sha1((key + WS_GUID).encode()).digest()).decode()
        conn.sendall((
            "HTTP/1.1 101 Switching Protocols\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            "Sec-WebSocket-Accept: %s\r\n\r\n" % accept).encode())
        Session(conn, args.marker, args.ticks, args.interval, logf).run()
    except OSError as exc:
        log("连接异常 %s：%s" % (addr, exc), logf)
    finally:
        try:
            conn.close()
        except OSError:
            pass


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--marker", default="p104-marker")
    ap.add_argument("--ticks", type=int, default=12)
    ap.add_argument("--interval", type=float, default=0.5)
    ap.add_argument("--log", default="")
    args = ap.parse_args()

    logf = open(args.log, "a", encoding="utf-8") if args.log else sys.stdout

    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        srv.bind((args.host, args.port))
    except OSError as exc:
        sys.stderr.write("无法监听 %s:%d：%s\n" % (args.host, args.port, exc))
        return 2
    srv.listen(32)
    log("上游已监听 %s:%d（marker=%s ticks=%d interval=%.2fs）"
        % (args.host, args.port, args.marker, args.ticks, args.interval), logf)

    try:
        while True:
            conn, addr = srv.accept()
            threading.Thread(target=serve, args=(conn, addr, args, logf), daemon=True).start()
    except KeyboardInterrupt:
        log("收到中断，退出", logf)
    finally:
        srv.close()
        if args.log:
            logf.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
