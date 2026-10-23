//go:build !unix

package apt

// 宿主（没有 flock 语义）上的探测结论（P11.2）。
//
// 这条断言在这里而不是 locks_test.go 里：它的答案是平台相关的——宿主必须说
// "读不到"，而 unix 实现对自己的锁文件说"空闲"。用一个平台无关的断言去锁
// 两个平台的行为，结果就是总有一个平台在说假话。

import (
	"errors"
	"testing"
)

func TestProbeLockUnsupportedIsErrorNotFree(t *testing.T) {
	held, err := probeLock("/var/lib/dpkg/lock-frontend")
	if err == nil {
		t.Fatal("这个平台没有 flock 语义，必须报读不到")
	}
	if held {
		t.Fatal("读不到不能算被占用")
	}
	if !errors.Is(err, ErrLockProbeUnsupported) {
		t.Fatalf("应返回 ErrLockProbeUnsupported，实际 %v", err)
	}
}
