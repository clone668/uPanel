package proxies

// 写配置用的路径与文件系统接缝（P10.2）。
//
// 与 internal/shares 的 ConfigFS 是同一套形状，但两棵树完全不同（/etc/samba 与
// /etc/nginx），共用一个抽象只会让两边互相牵制。这里重复的是二十行胶水，换来的是
// 每个模块都能独立地说清自己怎么落盘。
//
// 接缝存在的理由和 P08.2 一样：真机上 /etc/nginx 是 root 的，而“候选不合格时正式
// 文件一个字节都没动”“备用文件不存在时不留半个文件”这类不变量必须在单元测试里能
// 反复跑——它们不该只由一次虚拟机验收来担保。

import (
	"os"
	"path"
)

// 生产路径。全部是常量：面板执行哪个路径不由 HTTP 参数决定。
const (
	DefaultConfigRoot = "/etc/nginx"
	DefaultNginxPath  = "/usr/sbin/nginx"
	// DefaultBackupDir 存放每次替换前的旧文件。
	//
	// 放在 /var/lib/upanel-agent 而不是 /etc/nginx/upanel-backups：备份目录若在
	// 配置树里，nginx 的 `include conf.d/*.conf` 不会读到它（不同目录），但它会让
	// “配置树里有哪些文件”这件事多出一堆与运行无关的东西。备份是代理的财产。
	DefaultBackupDir = "/var/lib/upanel-agent/nginx-backups"

	// managedFileMode 是托管配置文件的权限。644 是 nginx 能读、别的进程能读但不能改。
	managedFileMode os.FileMode = 0o644
	// managedDirMode 是 conf.d / upanel-auth 目录的权限。
	managedDirMode os.FileMode = 0o755
)

// ConfigPaths 是写配置用到的路径。
//
// 生产不传它（用默认值）。它存在只为单元测试：让“临时目录里的一棵假配置树”能跑
// 完整条流水线，包括失败回滚。
type ConfigPaths struct {
	// Root 是配置根目录（/etc/nginx）。
	Root string
	// NginxPath 是 nginx 可执行文件（nginx -t / -s reload 用）。
	NginxPath string
	// BackupDir 是旧文件备份目录。
	BackupDir string
}

// ConfDir 返回 conf.d 目录。
//
// 用 path 而不是 filepath：这些字符串会**原样写进 nginx 配置**（auth_basic_user_file）
// 或者被当作 nginx 侧的绝对路径使用，它们属于一台 Linux 机器的配置树，格式是 POSIX 的。
// 用 filepath 会让“在哪台机器上编译”决定写进配置里的分隔符——那是一个只在 Windows
// 上编译时才发作、又只在 Linux 上运行时才看得见的错误。
func (p ConfigPaths) ConfDir() string { return path.Join(p.Root, "conf.d") }

// AuthDir 返回 htpasswd 目录（同样是 POSIX 路径，理由见 ConfDir）。
func (p ConfigPaths) AuthDir() string { return path.Join(p.Root, AuthDirName) }

// ConfigFS 是配置写入的文件系统接缝。
//
// 每个方法对应流水线上的一步，因此顺序本身可以被断言：WriteFileSync 不带 rename
// （候选要能被单独看见），Rename 单列一步，FsyncDir 单列一步（掉电后 rename 是否
// 持久取决于它）。
type ConfigFS struct {
	ReadFile      func(name string) ([]byte, error)
	WriteFileSync func(name string, data []byte, perm os.FileMode) error
	Rename        func(oldpath, newpath string) error
	FsyncDir      func(dir string) error
	MkdirAll      func(dir string, perm os.FileMode) error
	Remove        func(name string) error
	Stat          func(name string) (os.FileInfo, error)
	ReadDir       func(name string) ([]os.DirEntry, error)
	// Chown 用于把 htpasswd 交给 nginx 的 worker 用户读。nil 时跳过（测试）。
	Chown func(name string, uid, gid int) error
}

// SystemConfigFS 是真实的文件系统实现。
func SystemConfigFS() ConfigFS {
	return ConfigFS{
		ReadFile:      os.ReadFile,
		WriteFileSync: writeFileSync,
		Rename:        os.Rename,
		FsyncDir:      fsyncDir,
		MkdirAll:      func(dir string, perm os.FileMode) error { return os.MkdirAll(dir, perm) },
		Remove:        os.Remove,
		Stat:          os.Stat,
		ReadDir:       os.ReadDir,
		Chown:         os.Chown,
	}
}

// writeFileSync 写文件、把权限钉成 perm、然后 fsync。
//
// 与 shares 的同名函数同理：OpenFile 的 perm 只在创建时生效且会被 umask 掩掉，
// 而代理单元带 UMask=0077。文件权限是产品决定的事，不能由启动它的环境决定。
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

// fsyncDir 把目录项变化落盘。
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
