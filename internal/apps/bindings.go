package apps

import (
	"context"
	"fmt"
)

// BindingRef 是一条应用绑定的最小索引（P09.2 的发现合并用）。
//
// 只取“这是谁、绑在哪个对象上”三样东西，不带说明、图标、分组：候选列表要回答的是
// “这个目标已经被谁占了吗”，把整条应用元数据搬过来既没必要，也会让合并逻辑
// 顺手用上不该用的字段。
type BindingRef struct {
	AppID string
	Name  string
	Type  Type
	// Unit 是 systemd 绑定的单元名（type=systemd 时非空）。
	Unit string
	// ContainerID 是 docker 绑定的完整容器 ID（type=docker 时非空）。
	ContainerID string
	// DeploymentID 是 compose 绑定的部署标识（type=compose 时非空）。
	DeploymentID string
	// Health 是健康检查定义：发现页面据此说明“这个候选已经被启用/未启用健康检查地绑定了”。
	Health HealthCheck
}

// BindingIndex 返回全部应用的绑定索引。
//
// 不分页：应用数量是“这台服务器上我有几个入口”，不是“一屏能放几行”。真的到几千条时
// 需要的是分组与搜索（P09.5），而不是让发现页翻页——发现页的候选本来就整屏给出。
func (s *Service) BindingIndex(ctx context.Context) ([]BindingRef, error) {
	rows, err := s.query(ctx, `SELECT id, name, type, binding_json, health_json FROM apps ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("apps: 查询绑定索引失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]BindingRef, 0, 16)
	for rows.Next() {
		var (
			ref     BindingRef
			typ     string
			binding string
			health  string
		)
		if err := rows.Scan(&ref.AppID, &ref.Name, &typ, &binding, &health); err != nil {
			return nil, fmt.Errorf("apps: 读取绑定索引失败: %w", err)
		}
		ref.Type = Type(typ)
		parsed, err := parseBinding(ref.Type, binding)
		if err != nil {
			// 库里存在一条读不出来的绑定是数据损坏，不是“这条应用没绑定”：
			// 静默跳过会让一条应用从发现页凭空消失。
			return nil, fmt.Errorf("apps: 应用 %s 的绑定无法解析: %w", ref.AppID, err)
		}
		ref.Unit = parsed.Unit
		ref.ContainerID = parsed.ContainerID
		ref.DeploymentID = parsed.DeploymentID
		hc, err := parseHealth(health)
		if err != nil {
			return nil, fmt.Errorf("apps: 应用 %s 的健康检查无法解析: %w", ref.AppID, err)
		}
		ref.Health = hc
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apps: 读取绑定索引失败: %w", err)
	}
	return out, nil
}
