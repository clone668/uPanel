package containers

import (
	"strings"
	"testing"

	"upanel/internal/deploy"
)

// TestDeploymentIDMatchesManagementSide 钉住一件事：观测侧算出来的项目标识
// 必须与管理侧（deploy.DeploymentIDForName）逐字相同。
//
// 两个算法一旦分叉，宿主上跑着的项目在面板里永远“查不到”，而面板上的部署
// 也永远关联不到容器——这种错不会报错，只会让界面长期显示两个互不相干的列表
// （CONTRACTS §8.9：前端按下发的 deployment_id 把两张表连起来）。
//
// 这条断言曾经住在 internal/deploy 里。P15.4 加进 agentclient → deploy 的依赖之后，
// 它从那边引 containers 会成环，于是搬到这边来：能同时引两边的包只剩这一侧。
func TestDeploymentIDMatchesManagementSide(t *testing.T) {
	for _, name := range []string{"webapp", "a", "my-app_1", strings.Repeat("x", deploy.MaxNameLen)} {
		if got, want := ProjectID(name), deploy.DeploymentIDForName(name); got != want {
			t.Fatalf("项目名 %q：观测侧 %s，管理侧 %s", name, got, want)
		}
	}
}
