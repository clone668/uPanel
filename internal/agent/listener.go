package agent

import (
	"context"
	"fmt"
	"net"
)

// peerConn 是带内核对端身份的 Unix 连接。
type peerConn struct {
	net.Conn
	peer peerInfo
}

// peerListener 在 Accept 阶段就读取对端身份：连接一旦建立，身份即固定，
// 后面的每次请求都使用同一份凭据，不存在“先连上再切换身份”的窗口。
type peerListener struct {
	net.Listener
}

func (l peerListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("代理 socket 收到了非 Unix 连接 %T", conn)
	}
	info, err := peerCred(unixConn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("读取连接对端身份失败: %w", err)
	}
	return &peerConn{Conn: unixConn, peer: info}, nil
}

// connContext 把 Accept 阶段读到的身份放进请求上下文，供 gate 使用。
func connContext(ctx context.Context, conn net.Conn) context.Context {
	pc, ok := conn.(*peerConn)
	if !ok {
		return ctx
	}
	return withPeerInfo(ctx, pc.peer)
}
