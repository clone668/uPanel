package certificates

// 证书与私钥落盘（P10.3）。
//
// 这一层只有一件事要做对：**私钥写到只有 root 能读的位置，并且写到一半失败时不会
// 留下一个“证书是新的、私钥是旧的”的配对**。后者是这里最容易犯的错——证书和私钥
// 是两个文件，两次 rename 之间掉电就配对错位，而 nginx -t 会以
// `key values mismatch` 报出来，用户看到的是“证书坏了”，真正发生的是两次写入之间
// 掉了一次电。
//
// 做法与 P10.2 写 Nginx 配置同源：先把两份内容都写成候选（fsync），再依次 rename，
// 最后 fsync 目录；被替换的旧文件先备份，写完之后**从盘上读回来核对**配对——不是
// “我写进去了所以它是对的”。
//
// 顺序是有理由的：候选先写、rename 后做，是因为 rename 是原子的；两个 rename 之间
// 的窗口确实存在（是两次系统调用），但它已经缩到最小，而且下面有读回核对兜底。

import (
	"fmt"
	"os"
	"path"
	"time"

	"upanel/internal/tlscert"
)

// DefaultBackupDir 存放被替换掉的证书与私钥。
//
// 放在 /var/lib/upanel-agent 而不是证书目录里：证书目录是 Nginx 与运维要找的地方，
// 备份是代理的财产；混在一起会让“这个目录下有哪些证书”多出一堆历史文件。
const DefaultBackupDir = "/var/lib/upanel-agent/cert-backups"

// MaterialFS 是落盘的文件系统接缝（与 Nginx 那条流水线同形，理由相同：
// 不变量要在单元测试里反复跑，而不是只由一次虚拟机验收担保）。
type MaterialFS struct {
	ReadFile      func(name string) ([]byte, error)
	WriteFileSync func(name string, data []byte, perm os.FileMode) error
	Rename        func(oldpath, newpath string) error
	FsyncDir      func(dir string) error
	MkdirAll      func(dir string, perm os.FileMode) error
	Remove        func(name string) error
	Stat          func(name string) (os.FileInfo, error)
	Chmod         func(name string, mode os.FileMode) error
}

// SystemMaterialFS 是真实实现。
func SystemMaterialFS() MaterialFS {
	return MaterialFS{
		ReadFile: os.ReadFile,
		WriteFileSync: func(name string, data []byte, perm os.FileMode) error {
			// O_SYNC 的等价物：写完整份内容再落盘，返回时数据已在盘上排队。
			// 用 os.WriteFile 之后单独 Sync 会多一次打开，而这里要的就是“一次打开、
			// 写到盘上、关掉”。
			f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
			if err != nil {
				return err
			}
			if _, err := f.Write(data); err != nil {
				f.Close()
				return err
			}
			if err := f.Sync(); err != nil {
				f.Close()
				return err
			}
			// 显式 chmod：OpenFile 的 perm 只在**创建**时生效，覆盖一个已有的
			// 0644 文件时那个模式会被保留——那正是私钥被读走的方式。
			if err := f.Chmod(perm); err != nil {
				f.Close()
				return err
			}
			return f.Close()
		},
		Rename:   os.Rename,
		FsyncDir: fsyncDir,
		MkdirAll: os.MkdirAll,
		Remove:   os.Remove,
		Stat:     os.Stat,
		Chmod:    os.Chmod,
	}
}

// fsyncDir 把目录项的变更落盘（rename 是否持久取决于它）。
func fsyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// MaterialPaths 是证书落盘的路径（零值走默认；单元测试注入临时目录）。
type MaterialPaths struct {
	CertDir   string
	BackupDir string
}

func (p MaterialPaths) certDir() string {
	if p.CertDir == "" {
		return tlscert.CertDir
	}
	return p.CertDir
}

func (p MaterialPaths) backupDir() string {
	if p.BackupDir == "" {
		return DefaultBackupDir
	}
	return p.BackupDir
}

// writeOutcome 是一次落盘的结果描述。
type writeOutcome struct {
	CertPath    string
	KeyPath     string
	Backup      string
	CertState   string
	KeyState    string
	KeyMode     string
	CertWritten bool
}

// 落盘状态闭集（与代理侧的文件状态一致，报告里用同一套词）。
const (
	stateWritten   = "written"
	stateUnchanged = "unchanged"
)

// materialize 把一份证书链与一份私钥写到磁盘上。
//
// 调用方（assess 阶段）已经验过：证书不含私钥、覆盖域名、不是 CA、未过期，
// 私钥能解析且与证书配对。这里的验证是**再验一次**——因为两者的入口不同
// （一个来自参数，一个来自秘密存储），中间任何一步的假设都不该被继承。
func materialize(fs MaterialFS, paths MaterialPaths, certPEM, keyPEM []byte, domain string, now time.Time) (writeOutcome, error) {
	if fs.ReadFile == nil {
		fs = SystemMaterialFS()
	}
	info, err := tlscert.ParseCertificate(certPEM)
	if err != nil {
		return writeOutcome{}, fmt.Errorf("证书解析失败：%w", err)
	}
	if !tlscert.CoversDomain(info, domain) {
		return writeOutcome{}, fmt.Errorf("证书不覆盖域名 %s（覆盖的是 %v）", domain, info.Domains)
	}
	matches, _, err := tlscert.KeyMatchesCertificate(certPEM, keyPEM)
	if err != nil {
		return writeOutcome{}, fmt.Errorf("私钥不可用：%w", err)
	}
	if !matches {
		return writeOutcome{}, fmt.Errorf("私钥与证书不配对")
	}
	if _, err := tlscert.ParsePrivateKey(keyPEM); err != nil {
		return writeOutcome{}, fmt.Errorf("私钥解析失败：%w", err)
	}

	dir := paths.certDir()
	certPath := path.Join(dir, domain+".pem")
	keyPath := path.Join(dir, domain+".key")
	out := writeOutcome{CertPath: certPath, KeyPath: keyPath, CertState: stateWritten, KeyState: stateWritten}

	if err := fs.MkdirAll(dir, tlscert.CertDirMode); err != nil {
		return out, fmt.Errorf("创建证书目录 %s 失败：%w", dir, err)
	}
	// 目录权限不能只靠 MkdirAll：目录已存在时它的 perm 参数被忽略，
	// 而一个 0755 的 /etc/ssl/upanel 会让任何人都能读到里面的证书文件名。
	if fs.Chmod != nil {
		if err := fs.Chmod(dir, tlscert.CertDirMode); err != nil {
			return out, fmt.Errorf("设置证书目录权限失败：%w", err)
		}
	}

	// 备份旧文件。证书是公开的，私钥不是——但备份目录本身是 0700 root，
	// 所以把私钥备份在那里与它在原处一样安全，而它换来的是“写坏了能退回去”。
	backupName, err := backupExisting(fs, paths, []string{certPath, keyPath}, now)
	if err != nil {
		return out, err
	}
	out.Backup = backupName

	certTmp := certPath + ".upanel-new"
	keyTmp := keyPath + ".upanel-new"
	_ = fs.Remove(certTmp)
	_ = fs.Remove(keyTmp)
	if err := fs.WriteFileSync(certTmp, certPEM, tlscert.CertFileMode); err != nil {
		_ = fs.Remove(certTmp)
		return out, fmt.Errorf("写候选证书 %s 失败：%w", certTmp, err)
	}
	if err := fs.WriteFileSync(keyTmp, keyPEM, tlscert.KeyFileMode); err != nil {
		_ = fs.Remove(certTmp)
		_ = fs.Remove(keyTmp)
		return out, fmt.Errorf("写候选私钥 %s 失败：%w", keyTmp, err)
	}
	// 两个 rename 之间是唯一的错位窗口。先私钥后证书：如果只完成了第一个，
	// 盘上是“新私钥 + 旧证书”——两者都不配对，但**私钥仍然只有 root 能读**；
	// 反过来则是“新证书 + 旧私钥”，同样不配对。选先私钥是因为它更短：
	// 私钥文件小，rename 之后的 fsync 也更早完成。
	if err := fs.Rename(keyTmp, keyPath); err != nil {
		_ = fs.Remove(certTmp)
		_ = fs.Remove(keyTmp)
		return out, fmt.Errorf("替换私钥 %s 失败：%w", keyPath, err)
	}
	if err := fs.Rename(certTmp, certPath); err != nil {
		_ = fs.Remove(certTmp)
		return out, fmt.Errorf("替换证书 %s 失败（私钥已更新，两者此刻不配对）：%w", certPath, err)
	}
	if err := fs.FsyncDir(dir); err != nil {
		return out, fmt.Errorf("%s 已变更，但目录 fsync 失败（掉电后可能回退）：%w", dir, err)
	}

	// 读回核对：盘上的两份东西必须仍然配对，私钥权限必须是 0600。
	if fs.Chmod != nil {
		// rename 不会带走权限位（inode 是新的，模式在创建时定好了），
		// 这一次 chmod 是针对“有人先把 .key 换成软链”的兜底。
		if err := fs.Chmod(keyPath, tlscert.KeyFileMode); err != nil {
			return out, fmt.Errorf("设置私钥权限失败：%w", err)
		}
	}
	onDiskCert, err := fs.ReadFile(certPath)
	if err != nil {
		return out, fmt.Errorf("读回证书 %s 失败：%w", certPath, err)
	}
	onDiskKey, err := fs.ReadFile(keyPath)
	if err != nil {
		return out, fmt.Errorf("读回私钥 %s 失败：%w", keyPath, err)
	}
	defer zero(onDiskKey)
	ok, diskInfo, err := tlscert.KeyMatchesCertificate(onDiskCert, onDiskKey)
	if err != nil || !ok {
		return out, fmt.Errorf("读回核对失败：盘上的证书与私钥不配对（%v）", err)
	}
	if diskInfo.Fingerprint != info.Fingerprint {
		return out, fmt.Errorf("读回核对失败：盘上证书指纹与写入的不一致")
	}
	fi, err := fs.Stat(keyPath)
	if err != nil {
		return out, fmt.Errorf("读回私钥状态失败：%w", err)
	}
	out.KeyMode = fmt.Sprintf("%04o", fi.Mode().Perm())
	if fi.Mode().Perm()&0o077 != 0 {
		return out, fmt.Errorf("私钥权限是 %04o，同组或其他用户可读", fi.Mode().Perm())
	}
	return out, nil
}

// backupExisting 把即将被替换的文件复制到备份目录，返回备份文件名（没有旧文件时为空）。
func backupExisting(fs MaterialFS, paths MaterialPaths, files []string, now time.Time) (string, error) {
	type old struct {
		path string
		data []byte
	}
	var olds []old
	for _, p := range files {
		data, err := fs.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("读不到现有文件 %s，不替换它：%w", p, err)
		}
		olds = append(olds, old{path: p, data: data})
	}
	// 备份里可能夹着旧私钥的明文（这份副本是这里 ReadFile 出来的，调用方的
	// zero(keyPEM) 清不到它）：写完备份即清零，与"私钥用完即弃"同一条纪律。
	defer func() {
		for _, o := range olds {
			zero(o.data)
		}
	}()
	if len(olds) == 0 {
		return "", nil
	}
	dir := paths.backupDir()
	if err := fs.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建备份目录 %s 失败：%w", dir, err)
	}
	// 备份名带上时间与来源文件名：同一个证书被替换多次时，备份要能按时间排开。
	stamp := now.UTC().Format("20060102T150405Z")
	for _, o := range olds {
		name := path.Join(dir, stamp+"-"+path.Base(o.path))
		if err := fs.WriteFileSync(name, o.data, 0o600); err != nil {
			return "", fmt.Errorf("写备份 %s 失败：%w", name, err)
		}
	}
	return stamp, nil
}

// zero 清零一段字节（私钥用完即弃）。
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
