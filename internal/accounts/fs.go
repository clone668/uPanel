package accounts

// 文件写入的底层原语（P14.5）。
//
// 与 internal/shares/configwrite.go 的同名函数是两套实现，因为要求不同：
// 共享那份写的是面板独占的托管配置文件（整份重写、不需要改属主），这份写的是
// **别人也会写的文件里的一小块**（authorized_keys 的托管区）与 **sudo 自己的目录**
// （sudoers.d 里 0440、root 属主）。所以这里多一个 Chown，且权限必须显式钉死。

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
)

// writeFileSync 写文件、把权限钉成 perm、然后 fsync。
//
// 为什么写完还要 Chmod：OpenFile 的 perm 只在**创建**时生效，而且会被进程 umask
// 掩掉。生产的代理单元照例带 UMask=0077，于是“0600 的 authorized_keys”在真机上
// 会落成 0600（对），而“0440 的 sudoers 规则”会落成 0400（错）——文件权限是产品
// 决定的事，不能由启动它的 shell 决定。
func writeFileSync(name string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// fsyncDir 把目录项的变化落盘。
//
// rename 成功但目录没 fsync 时，掉电后可能回到旧文件——对 authorized_keys 是
// “刚加的公钥没了”，对 sudoers 是“刚删的规则回来了”。
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// hashBytes 返回一段内容的 sha256（十六进制）。
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
