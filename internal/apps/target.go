package apps

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FindByBinding 按“绑在哪个对象上”查应用（P09.4 的启动门禁用）。
//
// 反查的键是规范化的 binding_json，而不是在 Go 里遍历比较：规范化正是
// 部分唯一索引 (type, binding_json) 成立的前提（见 validate.go 的 bindingJSON），
// 两处用同一个函数编码，写进去的字节和查出来的字节才是同一串。
//
// ok=false 表示这个单元/容器/部署不属于面板管理的任何应用。这时面板对它没有任何
// 依赖关系要守，也就不该拦一次启动——拦了会变成“面板不认识的东西一律不许动”。
func (s *Service) FindByBinding(ctx context.Context, t Type, b Binding) (App, bool, error) {
	if !t.Valid() {
		return App{}, false, fmt.Errorf("apps: 未知应用类型 %q", string(t))
	}
	key, err := bindingJSON(t, b)
	if err != nil {
		return App{}, false, err
	}
	var id string
	row := s.queryRow(ctx, `SELECT id FROM apps WHERE type = ? AND binding_json = ?`, string(t), key)
	switch err := row.Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return App{}, false, nil
	case err != nil:
		return App{}, false, fmt.Errorf("apps: 按绑定查应用失败: %w", err)
	}
	// favorite 与健康结论对门禁没有用，但走 Get 能保证“按绑定查到的应用”与
	// “按 id 查到的应用”是同一个形状（依赖卷、绑定解析、时间都读一遍）。
	app, err := s.Get(ctx, 0, id)
	if err != nil {
		return App{}, false, err
	}
	return app, true, nil
}
