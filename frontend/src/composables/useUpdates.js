import { computed, reactive, ref } from 'vue'
import { api } from '../api/client'

const readStorageKey = 'ipm_v2_read_announcements'
const updateState = reactive({
  loaded: false,
  loading: false,
  status: null,
  error: '',
})
const showChecking = ref(false)
const readAnnouncementIDs = ref(loadReadAnnouncementIDs())
let loadingPromise = null
let checkingTimer = null

function loadReadAnnouncementIDs() {
  try {
    const value = JSON.parse(localStorage.getItem(readStorageKey) || '[]')
    return Array.isArray(value) ? value.filter((item) => typeof item === 'string').slice(-200) : []
  } catch {
    return []
  }
}

function saveReadAnnouncementIDs() {
  localStorage.setItem(readStorageKey, JSON.stringify(readAnnouncementIDs.value.slice(-200)))
}

const announcements = computed(() => Array.isArray(updateState.status?.announcements) ? updateState.status.announcements : [])
const unreadAnnouncements = computed(() => {
  const read = new Set(readAnnouncementIDs.value)
  return announcements.value.filter((item) => item?.id && !read.has(item.id))
})
const currentVersion = computed(() => updateState.status?.current?.version || '2.1.2')
const currentCommit = computed(() => updateState.status?.current?.commit || 'unknown')

async function loadUpdates(force = false) {
  if (loadingPromise) return loadingPromise
  if (updateState.loaded && !force) return updateState.status

  updateState.loading = true
  updateState.error = ''
  showChecking.value = false
  checkingTimer = window.setTimeout(() => { showChecking.value = true }, 600)

  loadingPromise = api(`/api/update/status${force ? '?force=1' : ''}`)
    .then((status) => {
      updateState.status = status || null
      updateState.error = String(status?.error || '')
      updateState.loaded = true
      realRun.value = updateState.status?.run || null
      // 页面打开时如果正好有一次更新在跑（比如刚点了更新又刷了页），接着看它的进度。
      if (realRun.value?.running && !runPoller) startRunPolling('')
      return updateState.status
    })
    .catch((error) => {
      updateState.error = error.message
      throw error
    })
    .finally(() => {
      if (checkingTimer) window.clearTimeout(checkingTimer)
      checkingTimer = null
      showChecking.value = false
      updateState.loading = false
      loadingPromise = null
    })

  return loadingPromise
}

function markAnnouncementRead(id) {
  const value = String(id || '').trim()
  if (!value || readAnnouncementIDs.value.includes(value)) return
  readAnnouncementIDs.value = [...readAnnouncementIDs.value, value].slice(-200)
  saveReadAnnouncementIDs()
}

function markAllAnnouncementsRead() {
  const ids = announcements.value.map((item) => String(item?.id || '').trim()).filter(Boolean)
  readAnnouncementIDs.value = [...new Set([...readAnnouncementIDs.value, ...ids])].slice(-200)
  saveReadAnnouncementIDs()
}

// 更新的进度。更新器跑在计划任务里、在服务之外，而且中途会把这个服务停掉再起回来，
// 所以进度只能靠轮询后端（后端读盘上的状态文件）。这里的状态不是「本页面发出去的请求」。
const realRun = ref(null)
const updateApplyBusy = ref(false)
const applyError = ref('')
// 点了「立即更新」之后，更新器还在计划任务里排队、还没写出第一步，后端给的仍然是上一次
// 那条记录。照原样显示，就等于拿上一次的结局回答这一次的动作——看起来像刚才那一下没
// 生效，或者更糟：像被拒绝了。所以这中间先用一条本地的占位记录顶上。
const updatePending = ref(false)
const PENDING_RUN = Object.freeze({
  running: true,
  state: 'pending',
  step: '启动中',
  message: '已经请求更新，正在等更新器报出第一步',
  started_at: '',
  from_commit: '',
  to_commit: '',
  finished_at: '',
  stale: false,
})
const pendingTicksLimit = 40 // 40 × 1500ms ≈ 60 秒
let runPoller = null
let runPollMisses = 0
let pendingTicks = 0

// 占位只在「已经请求了、但属于这一次的那条记录还没出现」这段时间顶班；
// 新记录一到、请求失败、或等待超时，立刻让位。
const updateRun = computed(() => (updatePending.value ? PENDING_RUN : realRun.value))

function stopRunPolling() {
  if (runPoller) window.clearInterval(runPoller)
  runPoller = null
  updateApplyBusy.value = false
  updatePending.value = false
}

function startRunPolling(previousStartedAt, pending = false) {
  stopRunPolling()
  runPollMisses = 0
  pendingTicks = 0
  updateApplyBusy.value = true
  updatePending.value = pending

  const tick = async () => {
    let status
    try {
      status = await api('/api/update/status')
    } catch {
      // 更新器正要停掉这个服务、然后再起回来，所以这几秒轮询必然失败。这是预期内的，
      // 不是错误，继续等；只有长时间联系不上才停下来，并如实说明等待是被打断的。
      runPollMisses += 1
      if (runPollMisses >= 20) {
        applyError.value = '更新期间一直联系不上服务，已经停止等待。刷新页面可以看最后的状态。'
        stopRunPolling()
      }
      return
    }
    runPollMisses = 0
    updateState.status = status || null
    realRun.value = updateState.status?.run || null
    const run = realRun.value
    // 还是上一次留下的那条记录：等这一次的 started_at 出现再判断结果，
    // 否则会把上次的结局当成这次的。
    if (!run || run.started_at === previousStartedAt) {
      // 占位卡片不能一直顶下去：更新器要是根本没起来，得说清楚，而不是永远转圈。
      if (updatePending.value) {
        pendingTicks += 1
        if (pendingTicks >= pendingTicksLimit) {
          applyError.value = '已经请求更新，但等了 60 秒还没等到更新器报出第一步，已经停止等待。刷新页面可以看最后的状态。'
          stopRunPolling()
        }
      }
      return false
    }
    // 属于这一次的那条记录到了，占位让位。
    updatePending.value = false
    if (run.running) return false
    stopRunPolling()
    loadUpdates(true).catch(() => {})
  }

  runPoller = window.setInterval(tick, 1500)
  tick()
}

async function applyUpdate() {
  if (updateApplyBusy.value) return
  applyError.value = ''
  const previousStartedAt = updateState.status?.run?.started_at || ''
  // 先顶上占位卡片，再发请求：等请求回来才给反馈的话，这中间显示的还是上一次的结局。
  updatePending.value = true
  updateApplyBusy.value = true
  try {
    await api('/api/update/apply', { method: 'POST' })
  } catch (error) {
    // 请求被拒绝（比如后台任务正在跑）：撤掉占位，让真正的错误显示出来。
    updatePending.value = false
    updateApplyBusy.value = false
    throw error
  }
  startRunPolling(previousStartedAt, true)
}

export function useUpdates() {
  return {
    updateState,
    showChecking,
    announcements,
    unreadAnnouncements,
    currentVersion,
    currentCommit,
    loadUpdates,
    markAnnouncementRead,
    markAllAnnouncementsRead,
    updateRun,
    updatePending,
    updateApplyBusy,
    applyError,
    applyUpdate,
  }
}
