package agentclient

// 部署现场的读取端（P15.4）：deployments.list / deployments.get。
//
// 这一层只做两件事：把代理给的事实解成 deploy.Inspection*，以及核对形状。
// 不做判断、不缓存：部署页要回答的是"现在盘上和 systemd 里是什么样"，
// 一份缓存下来的旧摘要会让人对着一个已经不存在的版本点"更新"。
//
// 形状检查比别处更较真，因为它省掉的是一整页假数据：
//   - 响应里没有 deployments 这个键（不是空数组，是**没有**）→ UnavailableError。
//     空数组是"这台机器上还没有部署"，缺键是"代理没答"，两者在页面上差一整个结论。
//   - 一条现场缺 name / deployment_id / version → UnavailableError。少了 version，
//     更新与删除都带不回正确的令牌，界面会给出一个"点了就冲突"的按钮。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"upanel/internal/deploy"
)

// ListManagedDeployments 列出面板管理的全部部署现场。
func (c *Client) ListManagedDeployments(ctx context.Context) (deploy.InspectionList, error) {
	raw, err := c.Query(ctx, deploy.QueryListName, nil)
	if err != nil {
		return deploy.InspectionList{}, err
	}
	// 先解到指针，用来区分"字段存在且为空数组"与"字段根本不存在"。
	var probe struct {
		Deployments *[]deploy.Inspection `json:"deployments"`
		Truncated   bool                 `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return deploy.InspectionList{}, &UnavailableError{Err: fmt.Errorf("部署列表响应无法解析: %w", err)}
	}
	if probe.Deployments == nil {
		return deploy.InspectionList{}, &UnavailableError{
			Err: errors.New("部署列表响应缺少 deployments")}
	}
	var out deploy.InspectionList
	if err := json.Unmarshal(raw, &out); err != nil {
		return deploy.InspectionList{}, &UnavailableError{Err: fmt.Errorf("部署列表响应无法解析: %w", err)}
	}
	for i, item := range out.Deployments {
		if err := checkInspection(item); err != nil {
			return deploy.InspectionList{}, &UnavailableError{
				Err: fmt.Errorf("部署列表第 %d 条：%w", i+1, err)}
		}
	}
	return out, nil
}

// GetManagedDeployment 读一份部署的现场。代理回 404 时原样带出（*Error），
// 由接口层翻成 NOT_FOUND——那是一个真实的结论，不是"代理不可用"。
func (c *Client) GetManagedDeployment(ctx context.Context, name string) (deploy.Inspection, error) {
	trimmed := strings.TrimSpace(name)
	if !deploy.ValidDeploymentName(trimmed) {
		// 本地先拒一次：不合规的名字在代理侧也只会得到 400，而这里返回的错误
		// 不带上"代理说"的措辞，读起来就是"你填错了"。
		return deploy.Inspection{}, fmt.Errorf("部署名不合法：%q", name)
	}
	raw, err := c.Query(ctx, deploy.QueryGetName, map[string]string{"name": trimmed})
	if err != nil {
		return deploy.Inspection{}, err
	}
	var out deploy.Inspection
	if err := json.Unmarshal(raw, &out); err != nil {
		return deploy.Inspection{}, &UnavailableError{Err: fmt.Errorf("部署详情响应无法解析: %w", err)}
	}
	if err := checkInspection(out); err != nil {
		return deploy.Inspection{}, &UnavailableError{Err: err}
	}
	return out, nil
}

// checkInspection 核对一条现场的三个必填字段。
//
// 只查这三个：其余字段的空值都有确切含义（config_digest 空 = 没有配置、
// record 为 null = 没有记录），把它们也当成"坏响应"会把真实现场判成故障。
func checkInspection(item deploy.Inspection) error {
	switch {
	case strings.TrimSpace(item.Name) == "":
		return errors.New("部署现场缺少 name")
	case strings.TrimSpace(item.DeploymentID) == "":
		return fmt.Errorf("部署现场 %q 缺少 deployment_id", item.Name)
	case strings.TrimSpace(item.Version) == "":
		return fmt.Errorf("部署现场 %q 缺少 version", item.Name)
	}
	return nil
}
