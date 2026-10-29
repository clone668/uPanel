<script setup lang="ts">
// 网络页（P14.1）：网卡与地址、路由、监听端口、防火墙现状。
//
// 四块事实都来自代理的一次实时采集（`ip`、`/proc`、`ufw`），面板不缓存、不镜像。
// 这一页**只读**：改地址是 network.apply（P14.2），改规则是 firewall.apply（P14.3），
// 两者都要走预检 → 任务。所以页面上没有任何写按钮——不是还没接上，是这一阶段
// 本来就不该有。
//
// P14.2 在页面上加的是**一张卡**（NetworkApplyCard）：改 IPv4 的表单、待确认记录
// 的倒计时，以及"确认 / 立刻退回"两个出路。它读 `GET /network/config`，其余四块
// 事实不变。第 5 条查询失败**不算整页失败**：写路径未接线时它本来就会 503，那时
// 页面照常显示现状，只是把"不能改"换成一句"写入未启用"。
//
// 显示规则：
//   - 「能不能改」由上方的判定横幅回答，不可改时把原因逐条列出来；
//     一个灰掉的按钮后面没有原因，人只会以为面板坏了。
//   - `issues[]` 非空不是整页失败：列表照常渲染，另起一条说明哪一条来源没读到。
//     把 issues 丢掉，"这台机器没有监听端口"就成了假话。
//   - 载波为 null 显示「读不到」，`metric` 没设置显示「未设置」，`pid=0` 显示
//     「占用进程读不到」——三种"读不到"都不能渲染成"没有"。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NAlert, NCard, NTag } from 'naive-ui'

import { ApiError } from '@/api/client'
import { fetchFirewallConfig, type FirewallConfigState } from '@/api/firewall'
import {
  addressText,
  backendLabel,
  capabilityHeadline,
  carrierText,
  exposureLabel,
  fetchFirewall,
  fetchNetworkInterfaces,
  fetchNetworkConfig,
  fetchNetworkListeners,
  fetchNetworkRoutes,
  firewallHeadline,
  issueText,
  kindLabel,
  listenerOwnerText,
  managerLabel,
  metricText,
  type FirewallState,
  type NetworkConfigState,
  type NetworkInterfaceList,
  type NetworkIssue,
  type NetworkListenerList,
  type NetworkRouteList,
} from '@/api/network'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import FirewallApplyCard from '@/components/FirewallApplyCard.vue'
import NetworkApplyCard from '@/components/NetworkApplyCard.vue'
import StatePanel from '@/components/StatePanel.vue'
import { formatTimestamp } from '@/utils/format'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const interfaces = ref<NetworkInterfaceList | null>(null)
const routes = ref<NetworkRouteList | null>(null)
const listeners = ref<NetworkListenerList | null>(null)
const firewall = ref<FirewallState | null>(null)
// 写路径的入口状态（P14.2）。null = 读不到或代理没接写路径，卡片改说"未启用"。
const networkConfig = ref<NetworkConfigState | null>(null)
const configError = ref<string | null>(null)
// 防火墙写路径的入口状态（P14.3）。与网络那一份分开取、分开报错：
// 两个开关是独立的（`-network-writes` / `-firewall-writes`），一个没接线
// 不该让另一张卡也说"不能改"。
const firewallConfig = ref<FirewallConfigState | null>(null)
const fwConfigError = ref<string | null>(null)

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))

// 四条查询各自成败：一条读不到不该把另外三条一起藏起来。
const issues = computed<NetworkIssue[]>(() => [
  ...(interfaces.value?.issues ?? []),
  ...(routes.value?.issues ?? []),
  ...(listeners.value?.issues ?? []),
  ...(firewall.value?.issues ?? []),
])

const capability = computed(() => interfaces.value?.capability ?? null)
const firewallReasons = computed(() => firewall.value?.reasons ?? [])
const managerReasons = computed(() => firewall.value?.manager_reasons ?? [])

const collectedAt = computed(() => {
  const stamps = [
    interfaces.value?.collected_at,
    routes.value?.collected_at,
    listeners.value?.collected_at,
    firewall.value?.collected_at,
  ].filter((stamp): stamp is string => Boolean(stamp))
  return stamps.length > 0 ? formatTimestamp(stamps[0]) : ''
})

const defaultGateways = computed(() => routes.value?.gateways ?? [])

async function load(): Promise<void> {
  try {
    // 四条并行：它们互不依赖，串起来只会让页面白等四份 8 秒的上限。
    // allSettled 而不是 all：一条失败不该把另外三条的事实一起丢掉。
    const [ifaceRes, routeRes, listenRes, firewallRes, configRes, fwConfigRes] =
      await Promise.allSettled([
        fetchNetworkInterfaces(),
        fetchNetworkRoutes(),
        fetchNetworkListeners(),
        fetchFirewall(),
        fetchNetworkConfig(),
        fetchFirewallConfig(),
      ])

    const firstError =
      (ifaceRes.status === 'rejected' && ifaceRes.reason) ||
      (routeRes.status === 'rejected' && routeRes.reason) ||
      (listenRes.status === 'rejected' && listenRes.reason) ||
      (firewallRes.status === 'rejected' && firewallRes.reason) ||
      null

    // 一条都没拿到才算整页失败：那时页面确实什么都给不出来。
    if (
      ifaceRes.status === 'rejected' &&
      routeRes.status === 'rejected' &&
      listenRes.status === 'rejected' &&
      firewallRes.status === 'rejected' &&
      configRes.status === 'rejected' &&
      fwConfigRes.status === 'rejected'
    ) {
      if (firstError instanceof ApiError) {
        error.value = firstError
        phase.value = 'error'
        return
      }
      throw firstError
    }

    interfaces.value = ifaceRes.status === 'fulfilled' ? ifaceRes.value.data : null
    routes.value = routeRes.status === 'fulfilled' ? routeRes.value.data : null
    listeners.value = listenRes.status === 'fulfilled' ? listenRes.value.data : null
    firewall.value = firewallRes.status === 'fulfilled' ? firewallRes.value.data : null
    // 写路径读不到**不进 error**：未接线时它必然 503，而那一条不该让整页显示成
    // "有一项没能读到"。它只影响那张卡上的话。
    networkConfig.value = configRes.status === 'fulfilled' ? configRes.value.data : null
    configError.value =
      configRes.status === 'rejected'
        ? configRes.reason instanceof ApiError
          ? configRes.reason.message
          : '读不到写路径的入口状态'
        : null
    // 同理：防火墙写路径读不到也**不进 error**，它只影响那一张卡上的话。
    firewallConfig.value = fwConfigRes.status === 'fulfilled' ? fwConfigRes.value.data : null
    fwConfigError.value =
      fwConfigRes.status === 'rejected'
        ? fwConfigRes.reason instanceof ApiError
          ? fwConfigRes.reason.message
          : '读不到防火墙写路径的入口状态'
        : null
    error.value = firstError instanceof ApiError ? firstError : null
    phase.value = 'ready'
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  }
}

onMounted(load)
onBeforeUnmount(() => {
  // 没有轮询：网络状态的变化速度是"管理员敲一条 netplan apply"，
  // 定时刷新只会让页面上的数字跳来跳去。手动刷新按钮足够。
})
</script>

<template>
  <section class="net">
    <div class="net__head">
      <h1 class="net__title">网络</h1>
      <p v-if="collectedAt" class="net__meta">采集于 {{ collectedAt }}</p>
      <button class="net__refresh" type="button" @click="load">刷新</button>
    </div>

    <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取网络现状" />
    <StatePanel
      v-else-if="panelState && error && panelState !== 'error'"
      :state="panelState"
      :description="error.message"
      :code="error.code"
      :request-id="error.requestId"
      :retryable="retryablePanelState(panelState)"
      @retry="load"
    />
    <ApiErrorPanel v-else-if="error && !interfaces && !routes && !listeners && !firewall" :error="error" @retry="load" />

    <template v-else>
      <!-- 部分失败：一条查询报了错，另外三条照常显示。 -->
      <NAlert v-if="error" type="warning" :bordered="false" class="net__gap">
        有一项没能读到：{{ error.message }}
      </NAlert>

      <!-- 能不能改：判定归判定，原因必须逐条摆出来。 -->
      <NCard v-if="capability" size="small" class="net__card">
        <template #header>能不能由面板修改</template>
        <p class="net__headline">{{ capabilityHeadline(capability) }}</p>
        <p class="net__sub">配置归属：{{ backendLabel(capability.backend) }}</p>
        <ul v-if="capability.reasons.length > 0" class="net__reasons">
          <li v-for="reason in capability.reasons" :key="reason">{{ reason }}</li>
        </ul>
        <p v-else class="net__sub">
          可以改，但改地址走下面那张卡：预检 → 任务 → 120 秒内确认，不确认就自动退回。
        </p>
      </NCard>

      <!-- 修改地址（P14.2）：没有写入口时只说明一句，不给按钮。 -->
      <NetworkApplyCard v-if="networkConfig" :config="networkConfig" @changed="load" />
      <NCard v-else size="small" class="net__card">
        <template #header>修改 IPv4 配置</template>
        <p class="net__sub">
          {{ configError ?? '读不到写路径的入口状态' }}。这台机器上暂时不能从面板改地址。
        </p>
      </NCard>

      <!-- 网卡与地址 -->
      <NCard v-if="interfaces" size="small" class="net__card">
        <template #header>网卡与地址</template>
        <table class="net__table">
          <thead>
            <tr>
              <th>网卡</th>
              <th>类型</th>
              <th>状态</th>
              <th>MTU</th>
              <th>链路</th>
              <th>地址</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="iface in interfaces.interfaces" :key="iface.name">
              <td>
                <strong>{{ iface.name }}</strong>
                <NTag v-if="iface.physical" size="small" type="info">物理</NTag>
                <div v-if="iface.master" class="net__sub">挂在 {{ iface.master }} 下</div>
                <div v-if="iface.mac" class="net__sub">{{ iface.mac }}</div>
              </td>
              <td>{{ kindLabel(iface.kind) }}</td>
              <td>{{ iface.state }}</td>
              <td>{{ iface.mtu }}</td>
              <td>{{ carrierText(iface.carrier) }}</td>
              <td>
                <div v-for="addr in iface.addresses" :key="`${addr.family}-${addr.address}-${addr.prefix_len}`">
                  {{ addressText(addr) }}
                  <span class="net__sub">（{{ addr.scope }}）</span>
                </div>
                <span v-if="iface.addresses.length === 0" class="net__sub">没有读到地址</span>
              </td>
            </tr>
          </tbody>
        </table>
      </NCard>

      <!-- 路由 -->
      <NCard v-if="routes" size="small" class="net__card">
        <template #header>路由与默认网关</template>
        <p v-if="defaultGateways.length > 0" class="net__sub">
          出口：
          <span v-for="(gateway, index) in defaultGateways" :key="`${gateway.family}-${gateway.address}`">
            {{ index > 0 ? '、' : '' }}{{ gateway.address }}
            <template v-if="gateway.device">（{{ gateway.device }}）</template>
          </span>
        </p>
        <p v-else class="net__sub">没有读到默认网关</p>
        <table class="net__table">
          <thead>
            <tr>
              <th>目标</th>
              <th>下一跳</th>
              <th>接口</th>
              <th>优先级</th>
              <th>来源</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="route in routes.routes" :key="`${route.family}-${route.destination}-${route.device}-${route.gateway}`">
              <td>
                {{ route.destination }}
                <NTag v-if="route.default" size="small" type="warning">默认</NTag>
              </td>
              <td>{{ route.gateway ?? '直连' }}</td>
              <td>{{ route.device ?? '—' }}</td>
              <td>{{ metricText(route) }}</td>
              <td>{{ route.protocol ?? '—' }}</td>
            </tr>
          </tbody>
        </table>
      </NCard>

      <!-- 监听端口 -->
      <NCard v-if="listeners" size="small" class="net__card">
        <template #header>监听端口</template>
        <p v-if="listeners.truncated" class="net__sub">
          结果被上限截断：这里不是全部监听端口。
        </p>
        <p v-if="listeners.listeners.length === 0" class="net__sub">没有读到监听端口。</p>
        <table v-else class="net__table">
          <thead>
            <tr>
              <th>端口</th>
              <th>协议</th>
              <th>绑定地址</th>
              <th>范围</th>
              <th>占用进程</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in listeners.listeners" :key="`${item.protocol}-${item.family}-${item.address}-${item.port}`">
              <td><strong>{{ item.port }}</strong></td>
              <td>{{ item.protocol }}</td>
              <td>{{ item.address }}</td>
              <td>
                <NTag :type="item.wildcard ? 'warning' : 'default'" size="small">
                  {{ exposureLabel(item) }}
                </NTag>
              </td>
              <td>{{ listenerOwnerText(item) }}</td>
            </tr>
          </tbody>
        </table>
      </NCard>

      <!-- 防火墙 -->
      <NCard v-if="firewall" size="small" class="net__card">
        <template #header>防火墙</template>
        <p class="net__headline">{{ firewallHeadline(firewall) }}</p>
        <p class="net__sub">判定为：{{ managerLabel(firewall.manager) }}</p>
        <ul v-if="managerReasons.length > 0" class="net__reasons">
          <li v-for="reason in managerReasons" :key="reason">{{ reason }}</li>
        </ul>
        <p v-if="firewall.installed" class="net__sub">
          默认策略：入站 {{ firewall.default_incoming ?? '读不到' }}、
          出站 {{ firewall.default_outgoing ?? '读不到' }}、
          转发 {{ firewall.default_routed ?? '读不到' }}；日志 {{ firewall.logging ?? '读不到' }}
        </p>
        <ul v-if="firewallReasons.length > 0" class="net__reasons">
          <li v-for="reason in firewallReasons" :key="reason">{{ reason }}</li>
        </ul>
        <p v-if="firewall.rules.length === 0" class="net__sub">没有读到规则。</p>
        <table v-else class="net__table">
          <thead>
            <tr>
              <th>#</th>
              <th>动作</th>
              <th>方向</th>
              <th>目标</th>
              <th>来源</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="rule in firewall.rules" :key="`${rule.number}-${rule.raw}`">
              <td>{{ rule.number }}</td>
              <td>{{ rule.action }}</td>
              <td>{{ rule.direction }}</td>
              <td>{{ rule.to }}</td>
              <td>{{ rule.from }}</td>
            </tr>
          </tbody>
        </table>
      </NCard>

      <!-- 修改 UFW 规则（P14.3）：没有写入口时只说明一句，不给按钮。 -->
      <FirewallApplyCard v-if="firewallConfig" :config="firewallConfig" @changed="load" />
      <NCard v-else size="small" class="net__card">
        <template #header>修改 UFW 规则</template>
        <p class="net__sub">
          {{ fwConfigError ?? '读不到防火墙写路径的入口状态' }}。这台机器上暂时不能从面板改规则。
        </p>
      </NCard>

      <!-- 读不到的部分：单列出来，不和"没有"混在一起。 -->
      <NAlert v-if="issues.length > 0" type="info" :bordered="false" class="net__gap">
        <p>以下来源没有读到（其余内容照常显示）：</p>
        <ul class="net__reasons">
          <li v-for="issue in issues" :key="`${issue.source}-${issue.code}-${issue.detail ?? ''}`">
            {{ issueText(issue) }}
          </li>
        </ul>
      </NAlert>
    </template>
  </section>
</template>

<style scoped>
.net {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.net__meta,
.net__sub {
  color: var(--upanel-text-muted, var(--ink-2));
  font-size: var(--fs-sm);
  margin: 4px 0 0;
}

.net__headline {
  margin: 0 0 4px;
  font-weight: 600;
}

.net__refresh {
  margin-left: auto;
  border: 1px solid var(--upanel-border, var(--rule));
  background: transparent;
  border-radius: var(--r-2);
  padding: 2px 10px;
  cursor: pointer;
}

.net__reasons {
  margin: 8px 0 0;
  padding-left: 18px;
  color: var(--upanel-text-muted, var(--ink-2));
  font-size: var(--fs-sm);
}

.net__table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--fs-md);
}

.net__table th,
.net__table td {
  text-align: left;
  padding: 6px 8px;
  border-bottom: 1px solid var(--upanel-border, var(--rule));
  vertical-align: top;
}

.net__table th {
  font-weight: 600;
  color: var(--upanel-text-muted, var(--ink-2));
}

.net__gap {
  margin: 0;
}
</style>
