//go:build linux

package network

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这个文件是给**真机**用的：它不注入任何假文件系统，用的就是生产里的 `osWriteFS`
// 与 `ApplyEngine.writeFileAtomic`。它的存在理由只有一个——P16.3 修掉的 Q505
// （rename 之后没刷父目录）在单测里只能证明"调用发生了"（假 FS 记了一笔），
// 证明不了 `os.File.Sync()` 打在一个**目录** fd 上在真文件系统上不报 EINVAL/EBADF。
// 那一步失败会让每一次网络写入在 `netplan apply` 之前就报错停下，是只能在真机上看的。
//
// 默认跳过：宿主与常规 `go test ./...` 不跑它（也不该跑——它写的是真实目录）。
// 真机上这样跑（宿主交叉编译，客户机没有 Go 工具链）：
//
//	GOOS=linux GOARCH=amd64 go test -c -o probe.test ./internal/network
//	sudo UPANEL_REAL_FS_DIR=/var/tmp/upanel-fsync-probe ./probe.test -test.run RealMachine -test.v
//
// **它证明的是"这条写路径在真机 ext4 上走得通、目录 fsync 不报错、内容与权限对"**，
// 不证明掉电语义（那要拔电源，仍是 NOT RUN）。
func TestRealMachineAtomicWriteSyncsDir(t *testing.T) {
	dir := os.Getenv("UPANEL_REAL_FS_DIR")
	if dir == "" {
		t.Skip("只在真机上跑：UPANEL_REAL_FS_DIR 未设置")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录 %s 失败：%v", dir, err)
	}

	fs := osWriteFS{}

	// 1. 目录 fsync 本身：这是 Q505 新加的那一步。打不开或用不了会在这里暴露。
	if err := fs.SyncDir(dir); err != nil {
		t.Fatalf("SyncDir(%s) 在真文件系统上失败：%v", dir, err)
	}

	// 2. 生产的原子写：临时文件 + rename + 刷父目录。
	e := &ApplyEngine{fs: fs}
	target := filepath.Join(dir, "90-upanel-probe.yaml")
	body := []byte("network:\n  version: 2\n  ethernets:\n    probe0:\n      dhcp4: true\n")
	if err := e.writeFileAtomic(target, body, 0o600); err != nil {
		t.Fatalf("writeFileAtomic(%s) 失败：%v", target, err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读回 %s 失败：%v", target, err)
	}
	if string(got) != string(body) {
		t.Fatalf("内容不一致：写入 %d 字节，读回 %d 字节", len(body), len(got))
	}

	st, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat %s 失败：%v", target, err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("权限应 0600，实际 %04o", perm)
	}
	if st.Size() != int64(len(body)) {
		t.Fatalf("长度应 %d，实际 %d", len(body), st.Size())
	}

	// 3. 不留临时文件：`.upanel-tmp` 留在 /etc/netplan 里只是噪音，但留在记录目录里
	//    会让人以为有一份没写完的配置。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("列目录 %s 失败：%v", dir, err)
	}
	for _, ent := range entries {
		if strings.HasSuffix(ent.Name(), ".upanel-tmp") {
			t.Fatalf("留下了临时文件 %s", ent.Name())
		}
	}

	// 4. 覆盖写一次（第二次走到同一条路径，验证 rename 能顶掉已有文件）。
	body2 := []byte("network:\n  version: 2\n  ethernets: {}\n")
	if err := e.writeFileAtomic(target, body2, 0o600); err != nil {
		t.Fatalf("覆盖写失败：%v", err)
	}
	got2, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读回（覆盖后）失败：%v", err)
	}
	if string(got2) != string(body2) {
		t.Fatalf("覆盖后内容不一致：%q", string(got2))
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("清理 %s 失败：%v", dir, err)
	}
	t.Logf("真机写路径通过：%s（目录 fsync + 原子写 + 覆盖写，权限 0600）", dir)
}
