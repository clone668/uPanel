package agent

// 部署现场的两条只读查询（P15.4）：deployments.list 与 deployments.get。
//
// 它们与 §8.3 的 containers.projects 是**两个问题**，不合并：
//   - containers.projects 回答"这台机器上跑着什么"——事实来自 docker，里面混着
//     别人手工 `docker compose up` 起来的、面板不知道也管不了的项目；
//   - deployments.list 回答"面板管着什么"——事实来自 /etc/upanel/deploy 下的三个
//     文件与 systemd 的两个回答。
//
// 拿前者当"我的部署"，手工起的项目会变成一条点不动的部署；拿后者当"机器上跑着什么"，
// 正在跑但配置不在盘上的项目会凭空消失。前端按 `project name == deployment name`
// 把两张表连起来，谁也不冒充谁。
//
// 两条都不改状态、不拉镜像、不查 docker：一次列表刷新去做那些事，会让页面在 docker
// 卡住时整页转圈。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"upanel/internal/deploy"
	"upanel/internal/httpx"
)

// DeployInspector 是部署现场读取端（生产实现是 *deploy.Service）。
//
// 与 DeploySource 分开声明，而不是把两个方法塞进那六个里：能读出现场与能执行动作
// 是两件事。一台装了 systemd、配好了配置目录、但 docker 暂时不在的机器，仍然应该
// 说得出"这台机器上部署过什么、它们现在什么状态"——把它和写路径绑成一个接口，
// 那种机器上就只剩下一条 404。
type DeployInspector interface {
	Inspect(ctx context.Context, name string) (deploy.Inspection, error)
	List(ctx context.Context) (deploy.InspectionList, error)
}

// newDeploymentsListQuery 登记 deployments.list：无参数，读一遍配置目录。
func newDeploymentsListQuery(src DeployInspector) Query {
	return Query{
		Name: deploy.QueryListName,
		Decode: func(raw json.RawMessage) (any, error) {
			// 没有参数。空结构 + 严格解码让多余字段被拒绝，而不是被静默忽略——
			// 一条只读查询收到 {"path":"/"} 却照常返回，会让人以为参数生效了。
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return src.List(ctx)
		},
	}
}

// deploymentGetParams 是 deployments.get 的参数。
type deploymentGetParams struct {
	Name string `json:"name"`
}

// newDeploymentsGetQuery 登记 deployments.get：按名字读一份部署的现场。
//
// 名字在解码阶段就按部署名规则校验：一个含斜杠或空格的字符串不该被带进文件路径
// 拼接的那一层去拒。没有这份部署时回 404（notFound），而不是一份空结构——
// 空结构会被界面读成"有这份部署，但它什么都没有"。
func newDeploymentsGetQuery(src DeployInspector) Query {
	return Query{
		Name: deploy.QueryGetName,
		Decode: func(raw json.RawMessage) (any, error) {
			var p deploymentGetParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.Name = strings.TrimSpace(p.Name)
			if !deploy.ValidDeploymentName(p.Name) {
				return nil, fmt.Errorf("%w: name 必须是部署名（小写字母开头，只含小写字母、数字、"+
					"下划线、中划线，最长 63 字符）", ErrParamsInvalid)
			}
			return p, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(deploymentGetParams)
			if !ok {
				return nil, fmt.Errorf("参数类型不匹配: %T", params)
			}
			insp, err := src.Inspect(ctx, p.Name)
			if err != nil {
				if errors.Is(err, deploy.ErrNoDeployment) {
					return nil, deploymentNotFoundError{name: p.Name}
				}
				var validation *deploy.ValidationError
				if errors.As(err, &validation) {
					return nil, fmt.Errorf("%w: %s", ErrParamsInvalid, validation.Message)
				}
				return nil, err
			}
			return insp, nil
		},
	}
}

// deploymentNotFoundError 让"这个名字上没有部署"以 404 到达 Web。
//
// 不压成 500：那会让 Web 把"部署已经删掉了"翻译成"代理不可用"，
// 于是页面提示"稍后重试"，而重试一万次也不会有这份部署。
type deploymentNotFoundError struct {
	name string
}

func (e deploymentNotFoundError) Error() string {
	return "没有这个部署：" + e.name
}

func (e deploymentNotFoundError) HTTPStatus() (int, httpx.Code) {
	return http.StatusNotFound, httpx.CodeNotFound
}
