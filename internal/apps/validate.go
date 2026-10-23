package apps

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"upanel/internal/storage"
)

// NormalizeOptions 是校验时的环境开关。
type NormalizeOptions struct {
	// AllowLoopbackPublicURL 允许入口地址指向本机回环地址。
	//
	// 只有本机开发模式（mock）会打开它：那里浏览器和面板在同一台机器上，127.0.0.1
	// 是真实可达的。生产环境一律拒绝——把 127.0.0.1 当入口地址填进面板，等于给出
	// 一个“在你自己的电脑上打开面板所在机器的 localhost”的死链接（PLAN.md 第 11 节：
	// 浏览器访问地址禁止自动生成 localhost/127.0.0.1）。
	AllowLoopbackPublicURL bool
}

// 单元名、容器 ID、目标 ID 的形状。
var (
	// systemdUnitPattern：允许字母数字开头，随后是字母数字与 : _ . @ -，以 .service 结尾。
	//
	// 只认 .service：定时器、socket、target 的启停语义与“应用”不同，且 PLAN.md 第 11 节
	// 说的是“关联确切 service 单元”。这里不做前缀展开（nginx 不自动变成 nginx.service）。
	systemdUnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@\-]{0,199}\.service$`)

	// containerIDPattern：完整容器 ID，64 位十六进制。名称会随重建变化，不能当唯一依据。
	containerIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

	// deploymentIDPattern：面板创建的部署标识。
	deploymentIDPattern = regexp.MustCompile(`^dep_[0-9a-f]{32}$`)

	// groupIDPattern：分组标识。
	groupIDPattern = regexp.MustCompile(`^grp_[0-9a-f]{32}$`)

	// targetIDPattern：健康检查目标标识。
	//
	// 前缀就是“本机 / 容器”的判别结果，后缀是不透明标识。这个形状把主机名、IP、
	// 端口、URL 全部排除在外：127.0.0.1 匹配不了（没有 local_ / cntr_ 前缀），
	// 169.254.169.254 也匹配不了。目标由代理发现（P09.2），面板只引用。
	targetIDPattern = regexp.MustCompile(`^(local|cntr)_[a-z0-9][a-z0-9_-]{0,47}$`)
)

// NewAppID 生成一个应用标识（app_ + 32 位小写十六进制）。
func NewAppID() (string, error) { return newID(appIDPrefix) }

// ValidTargetID 判断一个健康检查/代理上游目标标识的形状是否合法。
//
// 反向代理（internal/proxies）也用这条形状限制上游：一个地方定义“什么能当目标”，
// 才不会出现“应用的健康检查拒绝的目标，代理却接受”的错位。
func ValidTargetID(id string) bool { return targetIDPattern.MatchString(id) }

// ValidAppID 判断应用标识的形状（app_ + 32 位小写十六进制）。
func ValidAppID(id string) bool { return validAppID(id) }

// NewGroupID 生成一个分组标识（grp_ + 32 位小写十六进制）。
func NewGroupID() (string, error) { return newID(groupIDPrefix) }

func newID(prefix string) (string, error) {
	var b [idHexLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("apps: 生成标识失败: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// Normalize 校验并规范化一份元数据。
//
// 返回值才是可入库的形态：空白被裁掉、默认值被填好、依赖卷去重保序、绑定按类型判别。
// 任何不合法都返回 *ValidationError（400），不返回 500——输入问题不是系统故障。
//
// 组是否存在、卷是否存在需要数据库与代理，不在这里判断（见 Service）。
func (d Definition) Normalize(opts NormalizeOptions) (Definition, error) {
	out := d
	var err error

	if out.Name, err = normalizeName(d.Name); err != nil {
		return Definition{}, err
	}
	if out.Description, err = normalizeDescription(d.Description); err != nil {
		return Definition{}, err
	}
	if out.IconKey, err = normalizeIconKey(d.IconKey); err != nil {
		return Definition{}, err
	}
	if out.GroupID, err = normalizeGroupID(d.GroupID); err != nil {
		return Definition{}, err
	}
	if !out.Type.Valid() {
		return Definition{}, invalid("type", "应用类型必须是 link、systemd、docker 或 compose，收到 %q", string(d.Type))
	}
	if out.Binding, err = normalizeBinding(d.Type, d.Binding); err != nil {
		return Definition{}, err
	}
	if out.PublicURL, err = normalizePublicURL(d.PublicURL, opts); err != nil {
		return Definition{}, err
	}
	if out.Health, err = d.Health.normalize(); err != nil {
		return Definition{}, err
	}
	if out.VolumeDependencies, err = normalizeVolumeDependencies(d.VolumeDependencies); err != nil {
		return Definition{}, err
	}
	// link 的唯一意义就是“能点开”。没有入口的 link 在页面上是一张什么都没有的卡片。
	if out.Type == TypeLink && out.PublicURL == "" {
		return Definition{}, invalid("public_url", "link 类型的应用必须填写入口地址")
	}
	return out, nil
}

func normalizeName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", invalid("name", "应用名称不能为空")
	}
	if utf8.RuneCountInString(name) > MaxNameLen {
		return "", invalid("name", "应用名称最多 %d 个字符", MaxNameLen)
	}
	if hasControl(name) {
		return "", invalid("name", "应用名称不能包含控制字符或换行")
	}
	return name, nil
}

func normalizeDescription(raw string) (string, error) {
	desc := strings.TrimRight(raw, " \t\r\n")
	if utf8.RuneCountInString(desc) > MaxDescriptionLen {
		return "", invalid("description", "说明最多 %d 个字符", MaxDescriptionLen)
	}
	for _, r := range desc {
		// 说明是长文本，允许换行与制表；其余控制字符一律拒绝。
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return "", invalid("description", "说明不能包含控制字符")
		}
	}
	return desc, nil
}

func normalizeIconKey(raw string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if key == "" {
		return "", nil
	}
	for _, k := range BuiltinIconKeys {
		if k == key {
			return key, nil
		}
	}
	return "", invalid("icon_key", "图标只能是内置预设之一（%s），收到 %q", strings.Join(BuiltinIconKeys, "、"), raw)
}

func normalizeGroupID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", nil
	}
	if !groupIDPattern.MatchString(id) {
		return "", invalid("group_id", "分组标识必须是 grp_ 加 32 位小写十六进制")
	}
	return id, nil
}

// normalizeBinding 按类型严格判别绑定对象：该有的必须有，不该有的一个都不许有。
func normalizeBinding(t Type, b Binding) (Binding, error) {
	switch t {
	case TypeLink:
		if b != (Binding{}) {
			return Binding{}, invalid("binding", "link 类型的应用没有运行绑定，binding 必须是空对象")
		}
		return Binding{}, nil
	case TypeSystemd:
		if b.ContainerID != "" || b.DeploymentID != "" {
			return Binding{}, invalid("binding", "systemd 类型的绑定只能有 unit，不能带 container_id 或 deployment_id")
		}
		unit := strings.TrimSpace(b.Unit)
		if unit == "" {
			return Binding{}, invalid("binding.unit", "systemd 类型的绑定必须包含 unit")
		}
		if !systemdUnitPattern.MatchString(unit) || strings.Contains(unit, "..") {
			return Binding{}, invalid("binding.unit",
				"unit 必须是确切的服务单元名（例如 nginx.service），只能含字母数字与 : _ . @ -")
		}
		return Binding{Unit: unit}, nil
	case TypeDocker:
		if b.Unit != "" || b.DeploymentID != "" {
			return Binding{}, invalid("binding", "docker 类型的绑定只能有 container_id，不能带 unit 或 deployment_id")
		}
		id := strings.TrimSpace(b.ContainerID)
		if !containerIDPattern.MatchString(id) {
			return Binding{}, invalid("binding.container_id",
				"container_id 必须是容器的完整 ID（64 位十六进制）；容器名称会随重建变化，不能作为绑定依据")
		}
		return Binding{ContainerID: strings.ToLower(id)}, nil
	case TypeCompose:
		if b.Unit != "" || b.ContainerID != "" {
			return Binding{}, invalid("binding", "compose 类型的绑定只能有 deployment_id，不能带 unit 或 container_id")
		}
		id := strings.TrimSpace(b.DeploymentID)
		if !deploymentIDPattern.MatchString(id) {
			return Binding{}, invalid("binding.deployment_id",
				"deployment_id 必须是面板创建的部署标识（dep_ 加 32 位小写十六进制）；外部 Compose 项目只能只读发现")
		}
		return Binding{DeploymentID: id}, nil
	}
	return Binding{}, invalid("type", "应用类型必须是 link、systemd、docker 或 compose，收到 %q", string(t))
}

// normalizePublicURL 校验浏览器入口地址。
//
// 这个字段只描述“人从浏览器怎么打开它”，与健康检查/反向代理的内部目标完全无关：
// 面板不会去抓这个地址，也不会从它推导出探测目标。
func normalizePublicURL(raw string, opts NormalizeOptions) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if len(value) > MaxPublicURLLen {
		return "", invalid("public_url", "入口地址最多 %d 个字符", MaxPublicURLLen)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == ' ' {
			return "", invalid("public_url", "入口地址不能包含空白或控制字符")
		}
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", invalid("public_url", "入口地址不是合法的 URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", invalid("public_url", "入口地址只接受 http 或 https，收到 %q", u.Scheme)
	}
	if u.User != nil {
		return "", invalid("public_url", "入口地址不能包含用户名或密码")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", invalid("public_url", "入口地址缺少主机名")
	}
	if !opts.AllowLoopbackPublicURL {
		if isLoopbackHost(host) {
			return "", invalid("public_url",
				"入口地址不能是本机回环地址（%s）：浏览器打开它等于打开访问者自己的机器；本机目标请交给健康检查", host)
		}
		if host == "0.0.0.0" || host == "::" {
			return "", invalid("public_url", "%s 不是浏览器能访问的地址", host)
		}
	}
	return value, nil
}

// isLoopbackHost 判断主机名是否属于回环地址。
func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// normalize 校验并补齐健康检查定义。
func (h HealthCheck) normalize() (HealthCheck, error) {
	out := h

	switch scheme := strings.ToLower(strings.TrimSpace(h.Scheme)); scheme {
	case "":
		out.Scheme = "http"
	case "http", "https":
		out.Scheme = scheme
	default:
		return HealthCheck{}, invalid("health_check.scheme", "健康检查协议只允许 http 或 https，收到 %q", h.Scheme)
	}

	out.TargetID = strings.TrimSpace(h.TargetID)
	if out.TargetID != "" && !targetIDPattern.MatchString(out.TargetID) {
		return HealthCheck{}, invalid("health_check.target_id",
			"探测目标必须是代理发现的目标标识（local_… 或 cntr_…）；不接受主机名、IP、端口或 URL")
	}
	if out.Enabled && out.TargetID == "" {
		return HealthCheck{}, invalid("health_check.target_id", "启用健康检查必须指定探测目标")
	}

	out.Path = strings.TrimSpace(h.Path)
	if out.Path == "" {
		out.Path = "/"
	}
	if !strings.HasPrefix(out.Path, "/") || strings.HasPrefix(out.Path, "//") {
		return HealthCheck{}, invalid("health_check.path", "路径必须以单个 / 开头")
	}
	if strings.Contains(out.Path, "://") {
		return HealthCheck{}, invalid("health_check.path", "路径只能是路径，不能是完整 URL")
	}
	if len(out.Path) > MaxHealthPathLen {
		return HealthCheck{}, invalid("health_check.path", "路径最多 %d 个字符", MaxHealthPathLen)
	}
	if hasControl(out.Path) || strings.ContainsAny(out.Path, " \t") {
		return HealthCheck{}, invalid("health_check.path", "路径不能包含空白或控制字符")
	}

	if h.Port != 0 && (h.Port < 1 || h.Port > 65535) {
		return HealthCheck{}, invalid("health_check.port", "端口必须在 1 到 65535 之间，收到 %d", h.Port)
	}
	if out.Enabled && h.Port == 0 {
		return HealthCheck{}, invalid("health_check.port", "启用健康检查必须指定端口")
	}
	out.Port = h.Port

	out.IntervalSeconds = h.IntervalSeconds
	if out.IntervalSeconds == 0 {
		out.IntervalSeconds = DefaultIntervalSeconds
	}
	if out.IntervalSeconds < MinIntervalSeconds || out.IntervalSeconds > MaxIntervalSeconds {
		return HealthCheck{}, invalid("health_check.interval_seconds",
			"探测间隔必须在 %d 到 %d 秒之间，收到 %d", MinIntervalSeconds, MaxIntervalSeconds, h.IntervalSeconds)
	}
	out.TimeoutSeconds = h.TimeoutSeconds
	if out.TimeoutSeconds == 0 {
		out.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if out.TimeoutSeconds < MinTimeoutSeconds || out.TimeoutSeconds > MaxTimeoutSeconds {
		return HealthCheck{}, invalid("health_check.timeout_seconds",
			"超时必须在 %d 到 %d 秒之间，收到 %d", MinTimeoutSeconds, MaxTimeoutSeconds, h.TimeoutSeconds)
	}
	if out.TimeoutSeconds >= out.IntervalSeconds {
		return HealthCheck{}, invalid("health_check.timeout_seconds",
			"超时（%d 秒）必须小于探测间隔（%d 秒），否则下一次探测会叠在上一次上",
			out.TimeoutSeconds, out.IntervalSeconds)
	}

	codes, err := normalizeStatusCodes(h.ExpectedStatusCodes)
	if err != nil {
		return HealthCheck{}, err
	}
	out.ExpectedStatusCodes = codes
	return out, nil
}

// normalizeStatusCodes 去重、排序并校验状态码集合。空集合表示默认 200—399。
func normalizeStatusCodes(raw []int) ([]int, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[int]bool, len(raw))
	out := make([]int, 0, len(raw))
	for _, c := range raw {
		if c < 100 || c > 599 {
			return nil, invalid("health_check.expected_status_codes", "状态码必须在 100 到 599 之间，收到 %d", c)
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) > MaxStatusCodes {
		return nil, invalid("health_check.expected_status_codes", "期望状态码最多 %d 个", MaxStatusCodes)
	}
	sort.Ints(out)
	return out, nil
}

// normalizeVolumeDependencies 校验依赖卷标识并去重（保留输入顺序）。
//
// 这里只查形状——卷是不是真的存在必须问代理（Service 负责）。形状与代理侧的
// storage.ValidVolumeID 同源，直接复用同一个判据，避免两处各写一遍再慢慢漂开。
func normalizeVolumeDependencies(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > MaxVolumeDeps {
		return nil, invalid("volume_dependencies", "依赖卷最多 %d 个", MaxVolumeDeps)
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		// 去空白再判形状：前端传了带空格的 id 是输入问题，不该变成“卷不存在”。
		id = strings.TrimSpace(id)
		if !storage.ValidVolumeID(id) {
			return nil, invalid("volume_dependencies", "依赖卷标识必须是 vol_ 加 32 位小写十六进制")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// hasControl 判断字符串里是否有控制字符（含换行）。
func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// bindingJSON 把绑定对象编成库里的规范 JSON。
//
// 规范化是部分唯一索引 (type, binding_json) 能起作用的前提：同一个容器无论经过哪条
// 代码路径写进来，落库的字节都必须一样，否则唯一约束形同虚设。
func bindingJSON(t Type, b Binding) (string, error) {
	switch t {
	case TypeLink:
		return "{}", nil
	case TypeSystemd:
		return marshalCanonical(struct {
			Unit string `json:"unit"`
		}{b.Unit})
	case TypeDocker:
		return marshalCanonical(struct {
			ContainerID string `json:"container_id"`
		}{b.ContainerID})
	case TypeCompose:
		return marshalCanonical(struct {
			DeploymentID string `json:"deployment_id"`
		}{b.DeploymentID})
	}
	return "", fmt.Errorf("apps: 未知应用类型 %q", string(t))
}

// parseBinding 从库里读回绑定对象。严格解析：多一个键就是数据损坏，不当成宽容处理。
func parseBinding(t Type, raw string) (Binding, error) {
	switch t {
	case TypeLink:
		if raw != "{}" {
			return Binding{}, fmt.Errorf("link 的绑定必须是空对象，实际为 %q", raw)
		}
		return Binding{}, nil
	case TypeSystemd:
		var v struct {
			Unit string `json:"unit"`
		}
		if err := strictUnmarshal(raw, &v); err != nil {
			return Binding{}, err
		}
		return Binding{Unit: v.Unit}, nil
	case TypeDocker:
		var v struct {
			ContainerID string `json:"container_id"`
		}
		if err := strictUnmarshal(raw, &v); err != nil {
			return Binding{}, err
		}
		return Binding{ContainerID: v.ContainerID}, nil
	case TypeCompose:
		var v struct {
			DeploymentID string `json:"deployment_id"`
		}
		if err := strictUnmarshal(raw, &v); err != nil {
			return Binding{}, err
		}
		return Binding{DeploymentID: v.DeploymentID}, nil
	}
	return Binding{}, fmt.Errorf("未知应用类型 %q", string(t))
}

// healthJSON 把健康检查编成规范 JSON：字段顺序固定、无 omitempty、无多余键。
func healthJSON(h HealthCheck) (string, error) {
	codes := h.ExpectedStatusCodes
	if codes == nil {
		// nil 必须落成 []，不能落成 null：null 与“空集合”在 JSON 里是两种意思，
		// 而这里两者都表示“默认 200—399”，读回来不该因为写法的差别而不同。
		codes = []int{}
	}
	return marshalCanonical(struct {
		Enabled             bool   `json:"enabled"`
		TargetID            string `json:"target_id"`
		Scheme              string `json:"scheme"`
		Port                int    `json:"port"`
		Path                string `json:"path"`
		ExpectedStatusCodes []int  `json:"expected_status_codes"`
		IntervalSeconds     int    `json:"interval_seconds"`
		TimeoutSeconds      int    `json:"timeout_seconds"`
	}{
		Enabled:             h.Enabled,
		TargetID:            h.TargetID,
		Scheme:              h.Scheme,
		Port:                h.Port,
		Path:                h.Path,
		ExpectedStatusCodes: codes,
		IntervalSeconds:     h.IntervalSeconds,
		TimeoutSeconds:      h.TimeoutSeconds,
	})
}

// parseHealth 从库里读回健康检查定义。
func parseHealth(raw string) (HealthCheck, error) {
	var v struct {
		Enabled             bool   `json:"enabled"`
		TargetID            string `json:"target_id"`
		Scheme              string `json:"scheme"`
		Port                int    `json:"port"`
		Path                string `json:"path"`
		ExpectedStatusCodes []int  `json:"expected_status_codes"`
		IntervalSeconds     int    `json:"interval_seconds"`
		TimeoutSeconds      int    `json:"timeout_seconds"`
	}
	if err := strictUnmarshal(raw, &v); err != nil {
		return HealthCheck{}, err
	}
	return HealthCheck{
		Enabled:             v.Enabled,
		TargetID:            v.TargetID,
		Scheme:              v.Scheme,
		Port:                v.Port,
		Path:                v.Path,
		ExpectedStatusCodes: v.ExpectedStatusCodes,
		IntervalSeconds:     v.IntervalSeconds,
		TimeoutSeconds:      v.TimeoutSeconds,
	}, nil
}

// marshalCanonical 编出紧凑 JSON（json.Marshal 本身不转义 HTML 之外的东西，顺序按字段声明）。
func marshalCanonical(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("apps: 序列化失败: %w", err)
	}
	return string(raw), nil
}

// strictUnmarshal 严格解析 JSON：拒绝未知字段与尾部多余内容。
func strictUnmarshal(raw string, dst any) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("JSON 不合法: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return fmt.Errorf("首个 JSON 对象之后仍有内容")
	}
	return nil
}

// 应用列表游标。排序键是 (name, id)：name 允许重复，只靠名字翻页会跳行或重复。
func encodeAppCursor(name, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("app:" + name + "\x00" + id))
}

func decodeAppCursor(cursor string) (name, id string, err error) {
	if cursor == "" {
		return "", "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", "", ErrInvalidCursor
	}
	rest, ok := strings.CutPrefix(string(raw), "app:")
	if !ok {
		return "", "", ErrInvalidCursor
	}
	name, id, ok = strings.Cut(rest, "\x00")
	if !ok || name == "" || id == "" {
		return "", "", ErrInvalidCursor
	}
	return name, id, nil
}

// 分组列表游标。排序键是 (sort_order, id)。
func encodeGroupCursor(sortOrder int, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("grp:%08d:%s", sortOrder, id)))
}

func decodeGroupCursor(cursor string) (sortOrder int, id string, err error) {
	if cursor == "" {
		return 0, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", ErrInvalidCursor
	}
	rest, ok := strings.CutPrefix(string(raw), "grp:")
	if !ok {
		return 0, "", ErrInvalidCursor
	}
	num, rest, ok := strings.Cut(rest, ":")
	if !ok {
		return 0, "", ErrInvalidCursor
	}
	if _, err := fmt.Sscanf(num, "%08d", &sortOrder); err != nil {
		return 0, "", ErrInvalidCursor
	}
	if rest == "" {
		return 0, "", ErrInvalidCursor
	}
	return sortOrder, rest, nil
}
