package deploy

// 部署的记录（P15.3）：一份部署在磁盘上留下的“我是怎么定义的、现在跑的是哪一版”。
//
// 为什么必须有它：`deployment.apply` 落盘的是**渲染后的字节**，不是定义。拿一份
// Compose 配置反推定义（解析 YAML、再把明文口令重新塞回 secret_ref）既不可靠也危险，
// 而 `deployment.update` 要做的恰恰是“在同一份定义上只换镜像”。所以 P15.2 起每次
// 成功落地都顺手把**规范化后的定义**写在这里；它不含明文秘密（只有 sec_ 引用），
// 因此可以和单元文件一样放在 /etc/upanel/deploy 下，权限 0640。
//
// 三个文件各管一件事，谁也不替谁作证：
//
//	<name>.compose.yaml       渲染字节（0600，有明文秘密）——部署的“版本”就是它的 sha256
//	<name>.compose.yaml.prev  上一版的渲染字节（0600）——回切时逐字节还原，不重新渲染
//	<name>.deployment.json    定义 + 镜像 digest（0640）——更新要知道“原来是什么”
//
// 回切**不重新渲染**上一版：预检与执行之间秘密可能被轮换，重新渲染出来的字节
// 与上一版不等，那就不叫回切了。留着的那份字节是当时真正跑过的配置。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RecordSchema 是记录的格式版本。读到别的值不是“兼容一下”，而是拒绝：
// 一份来路不明的记录会让更新拿错定义。
const RecordSchema = 1

// recordFileMode / recordDirMode：定义与镜像记录里没有明文秘密，但仍只给 root。
//
// 0640 而不是 0644：定义里有环境变量名与公开值，那些组合起来能描述出这台机器上
// 跑着什么；运维要看单元文件，不需要看定义。
const recordFileMode = 0o640

// ImageRecord 是“某一版配置对应哪个镜像”的记录。
//
// 两个 digest 是两件事，别混：
//
//	Requested   定义里写的（nginx:1.27）——人话，可变
//	Digest      docker 报回来的镜像内容指纹（sha256:…）——这份镜像的唯一身份
//	RepoDigests 注册表给的 repo@sha256:… ——只有从注册表拉的才有；本地构建的没有
//
// 记 Digest 的目的只有一个：下一次更新时能说清“现在跑的和上一版不是同一个东西”，
// 以及“定义里钉住的 digest 与实际拉到的不是同一个”。`nginx:1.27` 这个 tag 昨天
// 与今天的字节可以不一样，digest 不会。
type ImageRecord struct {
	// Requested 是定义里写的镜像引用（规范化之后：没写 tag 的补上 :latest）。
	Requested string `json:"requested"`
	// Digest 是 `docker image inspect` 的 .Id（sha256: 加 64 位小写十六进制）。
	// 读不到时是空串——空串表示“没有读数”，不表示“一样”。
	Digest string `json:"digest,omitempty"`
	// RepoDigests 是注册表侧的 digest 列表。
	RepoDigests []string `json:"repo_digests,omitempty"`
	// ConfigDigest 是这一版渲染配置的摘要（与部署版本是同一个值）。
	ConfigDigest string `json:"config_digest"`
	// ResolvedAt 是读回这些事实的时刻。
	ResolvedAt time.Time `json:"resolved_at"`
}

// DeploymentRecord 是一份部署的完整记录。
type DeploymentRecord struct {
	Schema       int        `json:"schema"`
	DeploymentID string     `json:"deployment_id"`
	Name         string     `json:"name"`
	Definition   Definition `json:"definition"`
	// ConfigDigest 是记录这一版配置的摘要；与盘上的摘要不一致说明有人动过盘上的文件。
	ConfigDigest string      `json:"config_digest"`
	Image        ImageRecord `json:"image"`
	// Previous 是上一次成功的那一版（回切时告诉人“回得去的是哪一版”）。
	Previous  *ImageRecord `json:"previous,omitempty"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// Validate 检查一份记录能不能用来做更新。
//
// 严格到“多一个不对就整份不用”：更新是拿这份定义去渲染新配置、去覆盖盘上的部署，
// 记录里的名字对不上、定义又不自洽时，最安全的动作是拒绝，而不是猜。
func (r DeploymentRecord) Validate() error {
	if r.Schema != RecordSchema {
		return fmt.Errorf("deploy: 部署记录格式版本是 %d，本版本只认 %d", r.Schema, RecordSchema)
	}
	if !ValidDeploymentName(r.Name) {
		return fmt.Errorf("deploy: 部署记录里的名字 %q 不能拼进文件名", r.Name)
	}
	if want := DeploymentIDForName(r.Name); r.DeploymentID != want {
		return fmt.Errorf("deploy: 部署记录里的标识 %q 与名字 %q 派生出的 %q 不一致",
			r.DeploymentID, r.Name, want)
	}
	if strings.TrimSpace(r.Definition.Name) != r.Name {
		return fmt.Errorf("deploy: 部署记录里的定义名 %q 与部署名 %q 不一致",
			r.Definition.Name, r.Name)
	}
	if _, err := Normalize(r.Definition); err != nil {
		return fmt.Errorf("deploy: 部署记录里的定义不自洽: %w", err)
	}
	return nil
}

// RecordPath 返回部署记录的路径（生产目录）。
func RecordPath(name string) (string, error) {
	if !ValidDeploymentName(name) {
		return "", invalid(CodeMalformed, "name", "部署名 %q 不能拼进记录文件名", name)
	}
	return DeployConfigDir + "/" + name + ".deployment.json", nil
}

// PreviousConfigPath 返回上一版渲染配置的路径（生产目录）。
func PreviousConfigPath(name string) (string, error) {
	if !ValidDeploymentName(name) {
		return "", invalid(CodeMalformed, "name", "部署名 %q 不能拼进上一版配置文件名", name)
	}
	return DeployConfigDir + "/" + name + ".compose.yaml.prev", nil
}

// recordPath 是本 Applier 的配置目录下的记录路径。
func (a *Applier) recordPath(name string) (string, error) {
	full, err := RecordPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(a.configDir, filepath.Base(full)), nil
}

// previousConfigPath 是本 Applier 的配置目录下的上一版配置路径。
func (a *Applier) previousConfigPath(name string) (string, error) {
	full, err := PreviousConfigPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(a.configDir, filepath.Base(full)), nil
}

// configFile 是本 Applier 的配置目录下的配置路径。
func (a *Applier) configFile(name string) (string, error) {
	full, err := ConfigPath(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(a.configDir, filepath.Base(full)), nil
}

// ReadRecord 读一份部署记录。found 为 false 表示盘上没有（P15.2 创建的部署、
// 或记录被人删了），调用方必须据此拒绝更新，而不是当成“没有部署”。
func (a *Applier) ReadRecord(name string) (DeploymentRecord, bool, error) {
	var out DeploymentRecord
	path, err := a.recordPath(name)
	if err != nil {
		return out, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, false, nil
		}
		return out, false, fmt.Errorf("deploy: 读取部署记录 %s 失败: %w", path, err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, true, fmt.Errorf("deploy: 部署记录 %s 不是合法 JSON: %w", path, err)
	}
	return out, true, nil
}

// WriteRecord 原子地写一份部署记录。
func (a *Applier) WriteRecord(rec DeploymentRecord) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	path, err := a.recordPath(rec.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.configDir, configDirMode); err != nil {
		return fmt.Errorf("deploy: 创建配置目录 %s 失败: %w", a.configDir, err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("deploy: 序列化部署记录失败: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, recordFileMode); err != nil {
		return err
	}
	return nil
}

// RemoveRecord 删掉记录与上一版配置（删除部署时一并清掉）。
//
// 只删这两个我们自己写的文件：业务目录与数据卷不在这层的视野里，那条契约
// （CONTRACTS §5）不因为多了两个文件而改变。
func (a *Applier) RemoveRecord(name string) error {
	for _, pathOf := range []func(string) (string, error){a.recordPath, a.previousConfigPath} {
		path, err := pathOf(name)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("deploy: 删除 %s 失败: %w", path, err)
		}
	}
	return nil
}

// ReadPreviousConfig 读上一版渲染配置的字节。
func (a *Applier) ReadPreviousConfig(name string) ([]byte, bool, error) {
	path, err := a.previousConfigPath(name)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("deploy: 读取上一版配置 %s 失败: %w", path, err)
	}
	return data, true, nil
}

// SavePreviousConfig 把当前盘上的配置原样留一份（更新覆盖之前调用）。
//
// 传的是**读上来的字节**而不是重新渲染的结果：回切要还原的是当时真正跑过的那份
// 配置，而重新渲染会读一次此刻的秘密——秘密被轮换过，渲染出来的就不是上一版了。
func (a *Applier) SavePreviousConfig(name string, data []byte) error {
	path, err := a.previousConfigPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.configDir, configDirMode); err != nil {
		return fmt.Errorf("deploy: 创建配置目录 %s 失败: %w", a.configDir, err)
	}
	if err := writeFileAtomic(path, data, configFileMode); err != nil {
		return err
	}
	return nil
}

// ReadConfig 读当前渲染配置的字节与摘要（不存在时 found=false）。
func (a *Applier) ReadConfig(name string) ([]byte, string, bool, error) {
	path, err := a.configFile(name)
	if err != nil {
		return nil, "", false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", false, nil
		}
		return nil, "", false, fmt.Errorf("deploy: 读取配置 %s 失败: %w", path, err)
	}
	return data, digestOf(data), true, nil
}

// ErrRecordMissing 表示这份部署没有定义记录，无法更新。
//
// 它不是“没有这个部署”（单元与配置都在），而是“这份部署是旧版本创建的/记录被删了”。
// 两者对管理员的意义完全不同：前者要新建，后者只能删掉重来或手工恢复记录。
var ErrRecordMissing = errors.New("deploy: 这份部署没有定义记录，无法安全更新")

// requireRecord 读记录并校验，把三种失败分开说清楚。
func (a *Applier) requireRecord(name string) (DeploymentRecord, error) {
	rec, found, err := a.ReadRecord(name)
	if err != nil {
		return DeploymentRecord{}, err
	}
	if !found {
		return DeploymentRecord{}, fmt.Errorf("%w：%s 上没有 %s.deployment.json。"+
			"这份部署是 P15.2（只创建/删除）留下的，盘上没有它的定义；"+
			"要更新只能先 deployment.remove 再 deployment.apply 重建（数据不动）",
			ErrRecordMissing, a.configDir, name)
	}
	if err := rec.Validate(); err != nil {
		return DeploymentRecord{}, fmt.Errorf("%w: %v", ErrRecordMissing, err)
	}
	return rec, nil
}

// digestOf 与 configDigest 同一个口径（sha256:<hex>），只是作用于内存里的字节。
func digestOf(data []byte) string {
	return RenderedDigest(data)
}
