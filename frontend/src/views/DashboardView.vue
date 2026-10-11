<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Apple, ArrowRight, Boxes, CheckCircle2, CircleAlert, Inbox, LoaderCircle, MailCheck, MessageSquareText, ShieldCheck, Timer, Trash2 } from '@lucide/vue'
import { api } from '../api/client'
import { useConfirm } from '../composables/useConfirm'
import { subscribeRealtime } from '../composables/useRealtime'
import { useToast } from '../composables/useToast'

const loading = ref(true)
const loadFailed = ref(false)
const clearing = ref(false)
const dashboard = ref({ events: [] })
const runtimeTasks = ref([])
const currentTime = ref(Date.now())
const eventsViewport = ref(null)
const workspaceHeight = ref(576)
let countdownTimer
let runtimeRefreshTimer
let eventRefreshTimer
let realtimeRefreshTimer
let realtimeUnsubscribe = () => {}
let workspaceResizeTimer
let workspaceLayoutObserver
const pendingRealtimeResources = new Set()
let eventRefreshing = false
const { error: showError, success: showSuccess } = useToast()
const { confirm: confirmAction } = useConfirm()

const cards = computed(() => [
  { label: 'Apple 账号', value: dashboard.value.apple_account_count || 0, detail: `${dashboard.value.active_account_count || 0} 个状态正常`, icon: Apple, tone: 'text-slate-700 bg-slate-100 dark:bg-slate-700 dark:text-slate-200' },
  { label: '隐私邮箱', value: dashboard.value.mailbox_count || 0, detail: `${dashboard.value.available_count || 0} 个可用`, icon: Boxes, tone: 'text-emerald-600 bg-emerald-50 dark:bg-emerald-950/40 dark:text-emerald-300' },
  { label: '本地邮件', value: dashboard.value.message_count || 0, detail: '本地缓存的邮件记录', icon: MessageSquareText, tone: 'text-sky-600 bg-sky-50 dark:bg-sky-950/40 dark:text-sky-300' },
])

const operationalTasks = computed(() => {
  const taskIDs = ['imap-watcher', 'apple-keepalive', 'scheduler', 'public-api']
  return taskIDs.map((id) => runtimeTasks.value.find((task) => task.id === id)).filter(Boolean)
})

function taskIcon(task) {
  if (task.id === 'imap-watcher') return Inbox
  if (task.id === 'apple-keepalive') return Apple
  if (task.id === 'scheduler') return Timer
  return ShieldCheck
}

function taskStatusText(status) {
  return ({ completed: '已就绪', running: '运行中', starting: '启动中', creating: '创建中', waiting: '等待条件', failed: '运行异常', stopped: '已停止', idle: '未启动', planned: '待启用' })[status] || status
}

function taskStatusClass(status) {
  if (status === 'running' || status === 'creating') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-200'
  if (status === 'failed') return 'bg-rose-100 text-rose-700 dark:bg-rose-950/60 dark:text-rose-200'
  if (status === 'completed' || status === 'waiting' || status === 'starting') return 'bg-sky-100 text-sky-700 dark:bg-sky-950/60 dark:text-sky-200'
  return 'bg-slate-100 text-slate-500 dark:bg-slate-700 dark:text-slate-300'
}

function taskIconClass(status) {
  if (status === 'running' || status === 'creating') return 'bg-emerald-100 text-emerald-600 dark:bg-emerald-950/60 dark:text-emerald-300'
  if (status === 'failed') return 'bg-rose-100 text-rose-600 dark:bg-rose-950/60 dark:text-rose-300'
  if (status === 'completed' || status === 'waiting' || status === 'starting') return 'bg-sky-100 text-sky-600 dark:bg-sky-950/60 dark:text-sky-300'
  return 'bg-slate-100 text-slate-400 dark:bg-slate-700 dark:text-slate-300'
}

function countdownText(value) {
  if (!value) return '正在安排'
  const seconds = Math.max(0, Math.ceil((new Date(value).getTime() - currentTime.value) / 1000))
  if (!seconds) return '即将执行'
  return `${Math.floor(seconds / 60)}分${String(seconds % 60).padStart(2, '0')}秒`
}

function taskDescription(task) {
  if (task.id !== 'apple-keepalive' || task.status !== 'running') return task.description
  const jitter = task.jitter_percent ? ` · 每轮随机 ±${task.jitter_percent}%` : ''
  return `下次扫描 ${countdownText(task.next_run_at)}${jitter}`
}

async function refreshRuntimeTasks() {
  try {
    const taskData = await api('/api/tasks')
    runtimeTasks.value = taskData.items || []
  } catch {
    return
  }
}

async function refreshEvents() {
  if (eventRefreshing) return
  eventRefreshing = true
  try {
    const eventData = await api('/api/events')
    dashboard.value.events = eventData.items || []
  } catch {
    return
  } finally {
    eventRefreshing = false
  }
}

async function refreshDashboard() {
  try {
    dashboard.value = await api('/api/dashboard')
  } catch {
    return
  }
}

function scheduleRealtimeRefresh(change) {
	if (change.resource === 'event' && change.operation === 'created' && change.payload?.data) {
		dashboard.value.events = [change.payload.data, ...(dashboard.value.events || []).filter((item) => item.id !== change.payload.data.id)].slice(0, 30)
		return
	}
	if (change.resource === 'mailbox' && change.operation === 'batch-updated') {
		const created = Number(change.payload?.created_message_count || 0)
		if (created > 0) dashboard.value.message_count = Number(dashboard.value.message_count || 0) + created
		return
	}
  pendingRealtimeResources.add(change.resource)
  window.clearTimeout(realtimeRefreshTimer)
  realtimeRefreshTimer = window.setTimeout(() => {
    const resources = new Set(pendingRealtimeResources)
    pendingRealtimeResources.clear()
    if (resources.has('scheduler')) void refreshRuntimeTasks()
    if (resources.has('event')) void refreshEvents()
    if (['mailbox', 'message', 'apple-account'].some((resource) => resources.has(resource))) void refreshDashboard()
  }, 120)
}

function calculateWorkspaceHeight() {
  const element = eventsViewport.value
  if (!element || window.matchMedia('(max-width: 860px)').matches) return

  const scrollContainer = element.closest('.page-scroll')
  const scrollTop = scrollContainer?.scrollTop || 0
  const viewportTop = element.getBoundingClientRect().top + scrollTop
  const scrollBottom = scrollContainer?.getBoundingClientRect().bottom || window.innerHeight
  const scrollStyle = scrollContainer ? window.getComputedStyle(scrollContainer) : null
  const bottomPadding = Number.parseFloat(scrollStyle?.paddingBottom || '0') || 0
  const panelHeaderHeight = element.previousElementSibling?.getBoundingClientRect().height || 54
  const tableHeaderHeight = element.querySelector('thead')?.getBoundingClientRect().height || 36
  const dataRow = element.querySelector('tbody tr:not(.dashboard-events-empty-row)')
  const rowHeight = dataRow?.getBoundingClientRect().height || 43
  const tableWidth = element.querySelector('table')?.scrollWidth || 0
  const hasHorizontalScrollbar = tableWidth > element.clientWidth + 1
  const reservedHeight = (hasHorizontalScrollbar ? 8 : 0) + 1
  const availableHeight = scrollBottom - viewportTop - bottomPadding - 2
  const visibleRows = Math.max(5, Math.floor((availableHeight - tableHeaderHeight - reservedHeight) / rowHeight))
  const nextHeight = Math.floor(panelHeaderHeight + tableHeaderHeight + visibleRows * rowHeight + reservedHeight)

  if (nextHeight !== workspaceHeight.value) workspaceHeight.value = nextHeight
}

function scheduleWorkspaceHeight() {
  window.clearTimeout(workspaceResizeTimer)
  workspaceResizeTimer = window.setTimeout(calculateWorkspaceHeight, 80)
}

function eventTone(level) {
  if (level === 'error') return 'bg-rose-50 text-rose-600 dark:bg-rose-950/40 dark:text-rose-300'
  if (level === 'warning') return 'bg-amber-50 text-amber-600 dark:bg-amber-950/40 dark:text-amber-300'
  return 'bg-emerald-50 text-emerald-600 dark:bg-emerald-950/40 dark:text-emerald-300'
}

function formatTime(value) {
  if (!value) return '-'
  return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }).format(new Date(value))
}

async function clearEvents() {
  if (!dashboard.value.events?.length) return
  const confirmed = await confirmAction({
    title: '清空运行记录',
    message: '确定清空控制台的所有运行记录吗？清空后这些记录将不再显示。',
    confirmText: '确认清空',
    tone: 'danger',
  })
  if (!confirmed) return
  clearing.value = true
  try {
    await api('/api/events/clear', { method: 'POST', body: '{}' })
    dashboard.value.events = []
    showSuccess('控制台运行记录已清空')
  } catch (err) {
    showError(err.message)
  } finally {
    clearing.value = false
  }
}

onMounted(async () => {
  try {
    const [dashboardData, taskData] = await Promise.all([api('/api/dashboard'), api('/api/tasks')])
    dashboard.value = dashboardData
    runtimeTasks.value = taskData.items || []
  } catch (err) {
    loadFailed.value = true
    showError(err.message)
  } finally {
    loading.value = false
  }
  countdownTimer = window.setInterval(() => { currentTime.value = Date.now() }, 1000)
  realtimeUnsubscribe = subscribeRealtime(['scheduler', 'event', 'mailbox', 'message', 'apple-account'], scheduleRealtimeRefresh)
  runtimeRefreshTimer = window.setInterval(refreshRuntimeTasks, 30000)
  eventRefreshTimer = window.setInterval(refreshEvents, 30000)
  await nextTick()
  scheduleWorkspaceHeight()
  workspaceLayoutObserver = new ResizeObserver(scheduleWorkspaceHeight)
  const scrollContainer = eventsViewport.value?.closest('.page-scroll')
  if (scrollContainer) workspaceLayoutObserver.observe(scrollContainer)
  window.addEventListener('resize', scheduleWorkspaceHeight)
})

watch([loading, () => dashboard.value.events?.length], async () => {
  await nextTick()
  scheduleWorkspaceHeight()
})

onBeforeUnmount(() => {
  window.clearInterval(countdownTimer)
  window.clearInterval(runtimeRefreshTimer)
  window.clearInterval(eventRefreshTimer)
  window.clearTimeout(realtimeRefreshTimer)
  window.clearTimeout(workspaceResizeTimer)
  window.removeEventListener('resize', scheduleWorkspaceHeight)
  workspaceLayoutObserver?.disconnect()
  pendingRealtimeResources.clear()
  realtimeUnsubscribe()
})
</script>

<template>
  <div class="dashboard-page">
    <div v-if="loading" class="dashboard-loading"><LoaderCircle class="animate-spin" :size="18" />正在加载控制台</div>
    <div v-else-if="loadFailed" class="panel dashboard-load-error">控制台数据加载失败，请稍后刷新。</div>
    <template v-else>
      <div class="dashboard-stat-grid">
        <article v-for="card in cards" :key="card.label" class="panel dashboard-stat-card">
          <span :class="card.tone" class="dashboard-stat-icon"><component :is="card.icon" :size="17" /></span>
          <span class="dashboard-stat-copy"><span>{{ card.label }}</span><strong>{{ card.value }}</strong><small>{{ card.detail }}</small></span>
        </article>
      </div>

      <div class="dashboard-workspace" :style="{ '--dashboard-workspace-height': `${workspaceHeight}px` }">
        <section class="panel dashboard-events-panel">
          <header class="dashboard-section-heading"><div><h2>运行记录</h2><p>最近产生的系统事件</p></div><div><span>{{ dashboard.events?.length || 0 }} 条记录</span><button class="icon-button dashboard-clear-button" title="清空运行记录" :disabled="clearing || !dashboard.events?.length" @click="clearEvents"><LoaderCircle v-if="clearing" :size="14" class="animate-spin" /><Trash2 v-else :size="14" /></button></div></header>
          <div ref="eventsViewport" class="dashboard-events-viewport">
            <table class="dashboard-events-table" :class="{ 'dashboard-events-table-empty': !dashboard.events?.length }">
              <colgroup><col class="dashboard-event-column" /><col class="dashboard-event-column" /><col class="dashboard-event-column" /></colgroup>
              <thead><tr><th>事件</th><th>类型</th><th>时间</th></tr></thead>
              <tbody>
                <tr v-for="event in dashboard.events" :key="event.id">
                  <td><span class="dashboard-event-entry"><span :class="eventTone(event.level)" class="dashboard-event-icon"><CircleAlert v-if="event.level === 'error'" :size="12" /><CheckCircle2 v-else :size="12" /></span><span class="dashboard-event-message" :title="event.message">{{ event.message }}</span></span></td>
                  <td><span class="dashboard-event-category">{{ event.category }}</span></td>
                  <td><time>{{ formatTime(event.created_at) }}</time></td>
                </tr>
                <tr v-if="!dashboard.events?.length" class="dashboard-events-empty-row"><td colspan="3" class="dashboard-events-empty"><span><MailCheck :size="20" /></span><strong>暂无运行事件</strong><small>系统事件产生后会显示在这里。</small></td></tr>
              </tbody>
            </table>
          </div>
        </section>

        <aside class="panel dashboard-services-panel">
          <header class="dashboard-section-heading"><div><h2>核心服务</h2><p>后台任务实时状态</p></div><Inbox :size="16" /></header>
          <div v-if="operationalTasks.length" class="dashboard-service-list"><article v-for="task in operationalTasks" :key="task.id" class="dashboard-service-row"><span :class="taskIconClass(task.status)" class="dashboard-service-icon"><component :is="taskIcon(task)" :size="14" /></span><span class="dashboard-service-copy"><strong>{{ task.name }}</strong><small :title="taskDescription(task)">{{ taskDescription(task) }}</small></span><span :class="taskStatusClass(task.status)" class="dashboard-service-status">{{ taskStatusText(task.status) }}</span></article></div>
          <div v-else class="dashboard-service-empty">暂无运行状态</div>
          <footer><RouterLink to="/tasks" class="secondary-button dashboard-task-link">创建隐私邮箱 <ArrowRight :size="13" /></RouterLink></footer>
        </aside>
      </div>
    </template>
  </div>
</template>
