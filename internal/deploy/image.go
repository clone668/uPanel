package deploy

// 镜像的拉取与 digest 读数（P15.3）。
//
// 三件事必须分清，因为它们在真机上会给出三个不同的答案：
//
//	拉到了吗     `docker image pull` 的退出码说明这一次拉取成不成，不说明镜像有没有变。
//	             tag 是不可变的假象：`nginx:1.27` 今天与昨天可以是两份不同的镜像。
//	是哪一份     `docker image inspect` 的 .Id（sha256:…）——镜像内容的指纹。
//	注册表认哪一份 RepoDigests 里的 repo@sha256:…。本地构建的镜像没有这个，
//	             所以它是"补充证据"，不能当成唯一判据。
//
// 只用 argv 调 docker，从不拼 shell：镜像引用是用户给的字符串，虽然形状已经校验过
// （不含空白与控制字符），但"形状合法"与"能安全地拼进一条命令"是两件事。
//
// `--no-build` 已经在单元里写死（配置里也没有 build 段），所以这里只拉不建。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

const (
	// ImagePullTimeout 是一次拉镜像的时限。
	//
	// 600 秒与单元的 TimeoutStartSec 同一个量级：慢网络上一个几百 MB 的镜像就是这个
	// 数字。更新流程会在这个上限之内再按操作剩余预算收窄（见 update.go）。
	ImagePullTimeout = 600 * time.Second

	// ImageInspectTimeout 是读一次镜像事实的时限（只读本地镜像库，很快）。
	ImageInspectTimeout = 30 * time.Second

	// imageListLimit 是 docker 输出的上限。pull 的进度行很多，但它不该刷屏：
	// 超限说明这不是我们以为的那条命令。
	imagePullOutputLimit = 64 << 10
	imageInspectLimit    = 8 << 10
)

// ImageFacts 是本地镜像库里那个引用的实际身份。
type ImageFacts struct {
	// Requested 是请求的引用。
	Requested string
	// ID 是 `docker image inspect` 的 .Id。
	ID string
	// RepoDigests 是注册表侧的 repo@sha256:… 列表（可能为空）。
	RepoDigests []string
	// InspectedAt 是读这些事实的时刻。
	InspectedAt time.Time
}

// PullImage 拉一次镜像。
//
// 失败原样上抛：调用方（update）要据此判定"镜像没换、配置没写、单元还在旧版本上跑"，
// 而不是把它糊成一条通用的执行失败。
func (a *Applier) PullImage(ctx context.Context, reference string) error {
	ref := strings.TrimSpace(reference)
	if ref == "" {
		return errors.New("deploy: 要拉的镜像引用是空的")
	}
	res, err := a.run(ctx, syscmd.Spec{Program: a.docker, Args: []string{"image", "pull", ref}},
		syscmd.Limits{Timeout: ImagePullTimeout, MaxStdout: imagePullOutputLimit, MaxStderr: imagePullOutputLimit})
	var exitErr *syscmd.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return fmt.Errorf("deploy: 拉取镜像 %s 失败: %w", ref, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("deploy: 拉取镜像 %s 退出码 %d：%s", ref, res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// ImageFacts 读本地镜像库对这份引用的判断。
//
// 分两次问 `.Id` 与 `.RepoDigests`：一个 `--format` 模板里同时取两个字段要靠
// 模板里的 string join，而 docker 的模板函数在各版本里并不一致；两次调用换来的是
// "在哪个 docker 版本上都能跑"。
func (a *Applier) ImageFacts(ctx context.Context, reference string) (ImageFacts, error) {
	ref := strings.TrimSpace(reference)
	out := ImageFacts{Requested: ref, InspectedAt: time.Now().UTC().Truncate(time.Second)}
	if ref == "" {
		return out, errors.New("deploy: 要读的镜像引用是空的")
	}
	id, err := a.dockerInspect(ctx, ref, "{{.Id}}")
	if err != nil {
		return out, err
	}
	if !strings.HasPrefix(id, "sha256:") {
		return out, fmt.Errorf("deploy: docker 报回来的镜像 id %q 不是 sha256: 形态", id)
	}
	out.ID = id
	digests, err := a.dockerInspect(ctx, ref, "{{range .RepoDigests}}{{println .}}{{end}}")
	if err != nil {
		// RepoDigests 读不到不影响 .Id 这条主判据：本地构建的镜像本来就没有它。
		return out, nil
	}
	for _, line := range strings.Split(digests, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out.RepoDigests = append(out.RepoDigests, line)
		}
	}
	return out, nil
}

// dockerInspect 跑一次 `docker image inspect --format <tpl> <ref>`。
func (a *Applier) dockerInspect(ctx context.Context, ref, format string) (string, error) {
	res, err := a.run(ctx, syscmd.Spec{Program: a.docker,
		Args: []string{"image", "inspect", "--format", format, ref}},
		syscmd.Limits{Timeout: ImageInspectTimeout, MaxStdout: imageInspectLimit, MaxStderr: imageInspectLimit})
	var exitErr *syscmd.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return "", fmt.Errorf("deploy: 读镜像 %s 的事实失败: %w", ref, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("deploy: 读镜像 %s 的事实退出码 %d：%s", ref, res.ExitCode, firstLine(res.Stderr))
	}
	return strings.TrimSpace(res.Stdout), nil
}
