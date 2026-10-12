<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { AlertCircle, CheckCircle2, ChevronRight, ClipboardCopy, Cloud, ExternalLink, Inbox, KeyRound, Layers, LoaderCircle, Mail, MailOpen, Plus, RefreshCw, Save, Server, ShieldCheck, Trash2, X } from '@lucide/vue'
import { api } from '../api/client'
import { useConfirm } from '../composables/useConfirm'
import { subscribeRealtime } from '../composables/useRealtime'
import { useToast } from '../composables/useToast'
import AccountInboxPanel from '../components/AccountInboxPanel.vue'

const loading = ref(true)
const busyActions = ref([])
const showLogin = ref(false)
const showAddAlias = ref(false)
const data = ref({ items: [], module: {}, module_ready: true })
const selected = ref(null)
// 展开块里的三块分区默认都收起：别名一多，平铺着翻到最下面那块要很久。
const openSections = reactive({ auth: false, aliases: false })
// 授权信息就地编辑。留空＝回落到内置公共客户端，语义与系统设置页一致。
const authForm = reactive({ client_id: '', tenant: '', watcher_enabled: true })
// 「别名邮箱」列的是本机邮箱池里属于这个账号的记录，不是 Graph 的远端列表：
// 能在这里删掉的必须正好是池子里那一条，否则会出现「删了又冒出来」。
const poolMailboxes = ref([])
const login = reactive({ login_id: '', user_code: '', verification_uri: '', message: '', expires_at: '', status: 'idle' })
const aliasForm = reactive({ email: '', label: '', note: '' })

// 批量生成用独立的表单：前缀留空即纯随机，数量决定一次建多少个。
// 免费 Outlook.com 同样支持 + 标签寻址（实测确认），做法与谷歌一致。
const showBatch = ref(false)
const batchForm = reactive({ prefix: '', count: 100, label: '', note: '' })
const BATCH_MAX = 1000
// 「随机」按钮用的字符集，和后端保持一致。真正的随机后缀由服务端生成，
// 这里只决定前缀建议长什么样，所以两边不同步也不影响地址的正确性。
const PLUS_TAG_ALPHABET = 'abcdefghijklmnopqrstuvwxyz0123456789'

function randomPrefix() {
  const values = new Uint32Array(6)
  crypto.getRandomValues(values)
  return [...values].map((value) => PLUS_TAG_ALPHABET[value % PLUS_TAG_ALPHABET.length]).join('')
}

const batchPreview = computed(() => {
  const base = (selected.value?.email || 'you@outlook.com').split('@')[0].split('+')[0]
  const prefix = (batchForm.prefix || '').trim().toLowerCase()
  return `${base}+${prefix ? `${prefix}-xxxxxxxx` : 'xxxxxxxx'}@outlook.com`
})
const { success, error: showError } = useToast()
const { confirm: confirmAction } = useConfirm()
let pollTimer
let countdownTimer
let realtimeRefreshTimer
let realtimeUnsubscribe = () => {}
const remainingSeconds = ref(0)

const moduleReady = computed(() => data.value.module_ready !== false)
const moduleInfo = computed(() => data.value.module || {})
const polling = computed(() => login.status === 'pending' || login.status === 'slow_down')

function flash(text, isError = false) {
  if (isError) showError(text)
  else success(text)
}

function isBusy(action) {
  return busyActions.value.includes(action)
}

function startBusy(action) {
  if (!action || isBusy(action)) return false
  busyActions.value = [...busyActions.value, action]
  return true
}

function finishBusy(action) {
  if (!isBusy(action)) return
  busyActions.value = busyActions.value.filter((item) => item !== action)
}

function statusLabel(value) {
  return ({ active: '正常', need_login: '需要登录', need_consent: '需要授权', expired: '授权过期', rate_limited: '访问受限', failed: '失败' })[value] || value || '未知'
}

function statusClass(account) {
  const status = account.icloud_status || account.status
  if (status === 'active') return 'bg-emerald-50 text-emerald-600 dark:bg-emerald-950/30 dark:text-emerald-300'
  if (status === 'need_login' || status === 'need_consent' || status === 'expired') return 'bg-amber-50 text-amber-600 dark:bg-amber-950/30 dark:text-amber-300'
  return 'bg-rose-50 text-rose-600 dark:bg-rose-950/30 dark:text-rose-300'
}

function formatTime(value) {
  if (!value || String(value).startsWith('0001-')) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '—'
  return date.toLocaleString('zh-CN', { hour12: false })
}

function mailboxCount(account) {
  return account.mailbox_count ?? 0
}

async function load(options = {}) {
  const silent = Boolean(options.silent)
  if (!silent) loading.value = true
  try {
    data.value = await api('/api/microsoft-accounts')
    applyAuthForm()
    if (selected.value) {
      const current = (data.value.items || []).find((item) => item.id === selected.value.id)
      if (current) selected.value = { ...selected.value, ...current }
      else {
        selected.value = null
        poolMailboxes.value = []
      }
    }
  } catch (err) {
    flash(err.message, true)
  } finally {
    if (!silent) loading.value = false
  }
}

function scheduleRealtimeRefresh(change) {
  if (change.resource === 'microsoft-session') {
    window.clearTimeout(realtimeRefreshTimer)
    realtimeRefreshTimer = window.setTimeout(refreshAfterChange, 120)
    return
  }
  if (change.resource === 'apple-account' && change.operation === 'updated' && change.payload?.data?.id) {
    let applied = false
    data.value.items = (data.value.items || []).map((item) => {
      if (item.id !== change.payload.data.id) return item
      applied = true
      return { ...item, ...change.payload.data }
    })
    if (applied) return
  }
  window.clearTimeout(realtimeRefreshTimer)
  realtimeRefreshTimer = window.setTimeout(refreshAfterChange, 120)
}

function stopPolling() {
  window.clearInterval(pollTimer)
  window.clearInterval(countdownTimer)
  pollTimer = undefined
  countdownTimer = undefined
}

function tickCountdown() {
  if (!login.expires_at) {
    remainingSeconds.value = 0
    return
  }
  const deadline = new Date(login.expires_at).getTime()
  remainingSeconds.value = Math.max(0, Math.round((deadline - Date.now()) / 1000))
  if (remainingSeconds.value <= 0 && polling.value) {
    login.status = 'expired'
    stopPolling()
  }
}

function openLoginDialog() {
  login.login_id = ''
  login.user_code = ''
  login.verification_uri = ''
  login.message = ''
  login.expires_at = ''
  login.status = 'idle'
  remainingSeconds.value = 0
  showLogin.value = true
}

function closeLoginDialog() {
  stopPolling()
  showLogin.value = false
}

async function startLogin() {
  if (!startBusy('login-start')) return
  try {
    const result = await api('/api/microsoft-accounts/login/start', { method: 'POST' })
    login.login_id = result.login_id
    login.user_code = result.user_code
    login.verification_uri = result.verification_uri
    login.message = result.message || ''
    login.expires_at = result.expires_at
    login.status = 'pending'
    tickCountdown()
    const interval = Math.max(3, Number(result.interval_seconds) || 5) * 1000
    stopPolling()
    pollTimer = window.setInterval(pollLogin, interval)
    countdownTimer = window.setInterval(tickCountdown, 1000)
    flash('请在浏览器中打开验证地址并输入设备码')
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('login-start')
  }
}

async function pollLogin() {
  if (!login.login_id || isBusy('login-poll')) return
  startBusy('login-poll')
  try {
    const result = await api('/api/microsoft-accounts/login/poll', {
      method: 'POST',
      body: JSON.stringify({ login_id: login.login_id }),
    })
    if (result.status === 'pending' || result.status === 'slow_down') {
      login.status = result.status
      return
    }
    if (result.status === 'completed') {
      login.status = 'completed'
      stopPolling()
      const email = result.account?.apple_id || result.session?.email || ''
      flash(`已添加 Microsoft 账号：${email}`)
      closeLoginDialog()
      await load()
    }
  } catch (err) {
    stopPolling()
    login.status = 'failed'
    flash(err.message, true)
  } finally {
    finishBusy('login-poll')
  }
}

async function copyUserCode() {
  if (!login.user_code) return
  try {
    await navigator.clipboard.writeText(login.user_code)
    flash('设备码已复制')
  } catch {
    flash('复制失败，请手动选择复制', true)
  }
}

async function openVerificationPage() {
  if (!login.verification_uri) return
  window.open(login.verification_uri, '_blank', 'noopener')
}

// 点已经展开的那一行要收起。默认不选中任何行，也就是默认折叠。
async function toggleAccount(account) {
  if (selected.value?.id === account.id) {
    selected.value = null
    poolMailboxes.value = []
    return
  }
  await selectAccount(account)
}

async function selectAccount(account) {
  const busyKey = `detail:${account.id}`
  if (!startBusy(busyKey)) return
  try {
    const result = await api(`/api/microsoft-accounts/${account.id}`)
    selected.value = result.account
    poolMailboxes.value = result.mailboxes || []
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy(busyKey)
  }
}

// 只刷新邮箱池那一段。删掉别名或导入之后调它，不用重拉整个账号详情。
async function refreshPool() {
  if (!selected.value) {
    poolMailboxes.value = []
    return
  }
  try {
    const result = await api(`/api/microsoft-accounts/${selected.value.id}`)
    poolMailboxes.value = result.mailboxes || []
  } catch {
    // 刷新失败就保持原样，不要因此把已经显示出来的列表清空。
  }
}

// 账号列表和邮箱池是两份数据：load() 只重拉账号列表，池子那一段得单独刷。
// 任何一个会改动邮箱池的动作（导入别名、导入主地址、批量生成、手工添加）之后
// 都必须走这个，否则会出现「提示添加成功、列表里却看不见」。
async function refreshAfterChange() {
  await Promise.all([load({ silent: true }), refreshPool()])
}

function primaryAddress() {
  return String(selected.value?.email || selected.value?.apple_id || '').toLowerCase()
}

// 主地址那条池子记录由后端标成 microsoft_primary，比字符串比对可靠。
function isPrimaryMailbox(mailbox) {
  if (String(mailbox.remote_origin || '') === 'microsoft_primary') return true
  return String(mailbox.email || '').toLowerCase() === primaryAddress()
}

// 别名列表里不列主地址：点进来的这一行本身就是主地址，再列一遍是重复。
// 但它在邮箱池里的那条记录不能跟着删——Graph 是按收件人字段（To/Cc）去池子里
// 精确匹配的（protocol/graph_client.go 用池子记录建 byEmail），少了这条记录，
// 寄到主地址的信会被算成「未匹配」直接丢掉。所以这里只是不显示，不动数据。
const aliasMailboxes = computed(() => poolMailboxes.value.filter((mailbox) => !isPrimaryMailbox(mailbox)))

// 删除别名＝把这条记录从本机邮箱池里拿掉，连同它收到的邮件。
// Graph 没有删除别名的接口，微软账号上的地址本身删不掉——所以确认框里
// 必须把这一点说清楚，否则会让人以为连微软那边一起删了。
async function deleteAlias(mailbox) {
  const busyKey = `alias-delete:${mailbox.id}`
  if (!startBusy(busyKey)) return
  try {
    let count = -1
    try {
      const result = await api(`/api/mailboxes/${encodeURIComponent(mailbox.id)}/messages`)
      count = (result.items || []).length
    } catch {
      // 数不出来不该挡住删除，确认框里如实说数不出来。
    }
    const mailLine = count < 0 ? '它收到的邮件' : `它收到的 ${count} 封邮件`
    const confirmed = await confirmAction({
      title: '删除别名',
      message: `确定删除“${mailbox.email}”吗？\n\n会把它从本机邮箱池中移除，并一并删除${mailLine}。\n\n注意：这只是本机记录，Microsoft 账号上的这个别名本身不会被删除；再点一次「发现别名」还会把它读回来。`,
      confirmText: '删除',
      tone: 'danger',
    })
    if (!confirmed) return
    await api(`/api/mailboxes/${encodeURIComponent(mailbox.id)}?local_only=true`, { method: 'DELETE' })
    flash(`已从邮箱池删除 ${mailbox.email}`)
    await refreshAfterChange()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy(busyKey)
  }
}

// 授权信息表单跟随服务端返回的「生效值」：没填过时输入框留空、把内置兜底值
// 当占位符，否则第一次保存就会把兜底值写死成用户自己的选择。
//
// 只在首次加载和保存之后回填。load() 也会被实时事件触发，每次都回填会把
// 使用者正在输入的内容冲掉。
let authFormLoaded = false

function applyAuthForm(force = false) {
  if (authFormLoaded && !force) return
  const info = data.value.module || {}
  authForm.client_id = info.using_default_client ? '' : String(info.client_id || '')
  authForm.tenant = String(info.tenant || '') === 'common' ? '' : String(info.tenant || '')
  authForm.watcher_enabled = info.watcher_enabled !== false
  authFormLoaded = true
}

async function saveAuth() {
  if (!startBusy('save-auth')) return
  try {
    await api('/api/microsoft/settings', {
      method: 'PUT',
      body: JSON.stringify({
        client_id: authForm.client_id,
        tenant: authForm.tenant,
        watcher_enabled: authForm.watcher_enabled !== false,
      }),
    })
    await load({ silent: true })
    applyAuthForm(true)
    flash('Microsoft 授权信息已保存')
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('save-auth')
  }
}

async function deleteAccount(account) {
  const name = account.label || account.apple_id || account.id
  const confirmed = await confirmAction({
    title: '删除 Microsoft 账号',
    message: `确定删除“${name}”吗？\n\n本地 OAuth2 授权、关联别名和本地邮件会一并删除；Microsoft 账号本身不受影响。`,
    confirmText: '删除账号',
    tone: 'danger',
  })
  if (!confirmed) return
  const busyKey = `delete:${account.id}`
  if (!startBusy(busyKey)) return
  try {
    const result = await api(`/api/microsoft-accounts/${account.id}`, { method: 'DELETE' })
    if (selected.value?.id === account.id) {
      selected.value = null
      poolMailboxes.value = []
    }
    await load()
    const deleted = result.deleted || {}
    flash(`已删除 Microsoft 账号：${name}；清理别名 ${deleted.mailboxes || 0} 个，邮件 ${deleted.messages || 0} 封`)
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy(busyKey)
  }
}

async function checkAccount() {
  if (!selected.value) return
  if (!startBusy('check')) return
  try {
    await api(`/api/microsoft-accounts/${selected.value.id}/check`, { method: 'POST' })
    flash('Microsoft 授权校验通过')
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('check')
  }
}

async function discoverAliases() {
  if (!selected.value) return
  if (!startBusy('aliases')) return
  try {
    const result = await api(`/api/microsoft-accounts/${selected.value.id}/aliases`)
    // 这一段列表展示的是本机邮箱池，而「发现别名」只读 Graph、不动池子。
    // 所以必须在这里说清「发现了几个、其中几个还没进池」，否则点完屏幕上什么都不会变。
    const remote = result.aliases || []
    const inPool = new Set(poolMailboxes.value.map((item) => String(item.email || '').toLowerCase()))
    const missing = remote.filter((address) => !inPool.has(String(address).toLowerCase()))
    if (!remote.length) flash('该账号未返回代理地址，请手工添加别名')
    else if (missing.length) flash(`发现 ${remote.length} 个地址，其中 ${missing.length} 个还没进池，点「导入别名」写进来`)
    else flash(`发现 ${remote.length} 个地址，都已在本机邮箱池里`)
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('aliases')
  }
}

async function importAliases() {
  if (!selected.value) return
  if (!startBusy('import')) return
  try {
    const result = await api(`/api/microsoft-accounts/${selected.value.id}/aliases/import`, { method: 'POST' })
    flash(`别名导入完成：新增 ${result.created} 个，更新 ${result.updated} 个`)
    await refreshAfterChange()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('import')
  }
}

// 只导入主地址。微软的主地址由 proxyAddresses 的大写 SMTP 前缀标记，
// 后端已经能区分它和别名，所以这里可以像谷歌一样单独导入主地址。
async function importPrimary() {
  if (!selected.value) return
  if (!startBusy('import')) return
  try {
    const result = await api(`/api/microsoft-accounts/${selected.value.id}/aliases/import-primary`, { method: 'POST' })
    flash(`主地址导入完成：新增 ${result.created} 个，更新 ${result.updated} 个`)
    await refreshAfterChange()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('import')
  }
}

function openAddAliasDialog() {
  if (!selected.value) {
    flash('请先选择一个 Microsoft 账号', true)
    return
  }
  aliasForm.email = ''
  aliasForm.label = ''
  aliasForm.note = ''
  showAddAlias.value = true
}

function openBatchDialog() {
  if (!selected.value) {
    flash('请先选择一个 Microsoft 账号', true)
    return
  }
  batchForm.prefix = ''
  batchForm.count = 100
  batchForm.label = ''
  batchForm.note = ''
  showBatch.value = true
}

// 批量生成 + 标签地址。服务端会比对邮箱池里已有的全部地址，
// 所以这次建的 100 个和下次建的 1000 个不会重复。
async function generateAliases() {
  if (!selected.value) return
  const count = Number(batchForm.count)
  if (!Number.isFinite(count) || count < 1) {
    flash('创建数量必须是大于 0 的整数', true)
    return
  }
  if (count > BATCH_MAX) {
    flash(`单次最多生成 ${BATCH_MAX} 个地址`, true)
    return
  }
  if (!startBusy('generate-batch')) return
  try {
    const result = await api(`/api/microsoft-accounts/${selected.value.id}/aliases/generate-batch`, {
      method: 'POST',
      body: JSON.stringify({
        prefix: batchForm.prefix.trim(),
        count,
        label: batchForm.label.trim(),
        note: batchForm.note.trim(),
      }),
    })
    const skipped = result.skipped ? `，未能生成 ${result.skipped} 个` : ''
    flash(`已生成 ${result.created} 个 Outlook + 地址${skipped}`)
    showBatch.value = false
    await refreshAfterChange()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('generate-batch')
  }
}

async function addAlias() {
  if (!selected.value) return
  if (!startBusy('add-alias')) return
  try {
    const result = await api(`/api/microsoft-accounts/${selected.value.id}/mailboxes`, {
      method: 'POST',
      body: JSON.stringify(aliasForm),
    })
    showAddAlias.value = false
    flash(`${result.created ? '已添加' : '已更新'}别名：${result.mailbox.email}`)
    await refreshAfterChange()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('add-alias')
  }
}

onMounted(() => {
  load()
  realtimeUnsubscribe = subscribeRealtime(['microsoft-session', 'apple-account', 'mailbox'], scheduleRealtimeRefresh)
})

onBeforeUnmount(() => {
  stopPolling()
  window.clearTimeout(realtimeRefreshTimer)
  realtimeUnsubscribe()
})
</script>

<template>
  <div class="apple-account-page">
    <section class="panel apple-account-workbench">
      <header class="apple-command-bar">
        <div class="apple-command-title"><span><Mail :size="16" /></span><div><h2>Microsoft 账号</h2><p>通过 Microsoft Graph 读取 Outlook 别名的验证码</p></div></div>
        <div class="apple-command-actions">
          <button class="primary-button apple-command-button" :disabled="!moduleReady" :title="moduleReady ? '使用设备码登录 Microsoft 账号' : 'Microsoft 模块已在配置中关闭'" @click="openLoginDialog"><Plus :size="14" />添加 Microsoft 账号</button>
          <button class="secondary-button apple-command-button" :disabled="!selected || isBusy('aliases')" :title="selected ? '从 Graph 读取该账号的全部邮件地址' : '请先选择一个 Microsoft 账号'" @click="discoverAliases"><LoaderCircle v-if="isBusy('aliases')" :size="14" class="animate-spin" /><RefreshCw v-else :size="14" />发现别名</button>
          <button class="secondary-button apple-command-button" :disabled="!selected || isBusy('import')" :title="selected ? '只把该账号的主邮箱地址写入邮箱池' : '请先选择一个 Microsoft 账号'" @click="importPrimary"><LoaderCircle v-if="isBusy('import')" :size="14" class="animate-spin" /><Mail v-else :size="14" />导入主地址</button>
          <button class="secondary-button apple-command-button" :disabled="!selected || isBusy('import')" :title="selected ? '把发现的别名写入邮箱池' : '请先选择一个 Microsoft 账号'" @click="importAliases"><LoaderCircle v-if="isBusy('import')" :size="14" class="animate-spin" /><Inbox v-else :size="14" />导入别名</button>
          <button class="secondary-button apple-command-button" :disabled="!selected || isBusy('generate-batch')" :title="selected ? '随机批量生成 Outlook + 标签地址，跨批次不会重复' : '请先选择一个 Microsoft 账号'" @click="openBatchDialog"><LoaderCircle v-if="isBusy('generate-batch')" :size="14" class="animate-spin" /><Layers v-else :size="14" />批量生成</button>
        </div>
      </header>

      <div v-if="moduleInfo.alias_limit_note" class="apple-module-note">
        <AlertCircle :size="14" />
        <span>{{ moduleInfo.alias_limit_note }}</span>
      </div>

      <div v-if="loading" class="apple-loading"><LoaderCircle :size="16" class="animate-spin" />正在加载 Microsoft 账号</div>
      <div v-else class="apple-account-body">
        <section class="apple-account-list">
          <header class="apple-section-heading"><div><h3>账号与授权</h3><p>选择账号后可在右侧查看 OAuth2 授权与别名</p></div><span>{{ data.items?.length || 0 }} 个账号</span></header>
          <div v-if="!data.items?.length" class="apple-empty"><span><Server :size="20" /></span><strong>还没有 Microsoft 账号</strong><small>点击“添加 Microsoft 账号”，在浏览器中完成设备码登录。</small></div>
          <div v-else class="apple-account-table-viewport">
            <table class="apple-account-table">
              <colgroup><col class="apple-col-account" /><col class="apple-col-status" /><col class="apple-col-channel" /><col class="apple-col-action" /></colgroup>
              <thead><tr><th>Microsoft 账号</th><th>状态</th><th>读信通道</th><th>操作</th></tr></thead>
              <tbody>
                <template v-for="account in data.items" :key="account.id">
                <tr class="apple-account-row" :class="{ 'apple-account-row-selected': selected?.id === account.id }" tabindex="0" :aria-expanded="selected?.id === account.id" :aria-label="`查看 ${account.label || account.apple_id || 'Microsoft 账号'} 的授权详情`" @click="toggleAccount(account)" @keydown.enter="toggleAccount(account)" @keydown.space.prevent="toggleAccount(account)">
                  <td><button type="button" class="apple-account-select" :disabled="isBusy(`delete:${account.id}`) || isBusy(`detail:${account.id}`)" @click.stop="selectAccount(account)"><span class="apple-account-icon"><LoaderCircle v-if="isBusy(`detail:${account.id}`)" :size="14" class="animate-spin" /><Mail v-else :size="14" /></span><span><strong>{{ account.label || account.apple_id || 'Microsoft 账号' }}</strong><small>{{ account.apple_id || account.id }}</small></span></button></td>
                  <td><span :class="statusClass(account)" class="apple-status-badge">{{ statusLabel(account.icloud_status || account.status) }}</span></td>
                  <td><div class="apple-channel-list"><span class="apple-channel-pill"><Cloud :size="11" /><span>Microsoft Graph</span><em class="bg-emerald-50 text-emerald-600 dark:bg-emerald-950/30 dark:text-emerald-300">官方 API</em></span></div></td>
                  <td><button type="button" class="apple-delete-button" :title="`删除 ${account.label || account.apple_id || 'Microsoft 账号'}`" :aria-label="`删除 ${account.label || account.apple_id || 'Microsoft 账号'}`" :disabled="isBusy(`delete:${account.id}`)" @click.stop="deleteAccount(account)"><LoaderCircle v-if="isBusy(`delete:${account.id}`)" :size="13" class="animate-spin" /><Trash2 v-else :size="13" /></button></td>
                </tr>
                <tr v-if="selected?.id === account.id" class="apple-account-expand-row">
                  <td colspan="4" class="apple-account-expand-cell">
                    <div class="apple-account-expand-body">
                      <!-- ① 授权信息：折叠 + 就地可改。应用凭据不再在系统设置里重复展示。 -->
                      <section class="detail-section">
                        <header class="detail-section-head">
                          <button type="button" class="detail-section-toggle" :aria-expanded="openSections.auth" @click="openSections.auth = !openSections.auth">
                            <ChevronRight :size="15" class="detail-section-caret" />
                            <span class="detail-section-title">
                              <strong><ShieldCheck :size="15" />授权信息</strong>
                              <small>{{ selected.label || selected.apple_id }} · 客户端与租户可在这里改</small>
                            </span>
                          </button>
                          <div class="detail-section-actions">
                            <button class="secondary-button apple-check-button" :disabled="isBusy('check')" @click="checkAccount"><LoaderCircle v-if="isBusy('check')" :size="13" class="animate-spin" /><RefreshCw v-else :size="13" />{{ isBusy('check') ? '检查中' : '校验授权' }}</button>
                          </div>
                        </header>
                        <div v-if="openSections.auth" class="detail-section-body">
                          <div class="apple-state-list">
                            <article class="apple-state-row">
                              <span class="apple-state-icon text-sky-600 bg-sky-50 dark:bg-sky-950/30 dark:text-sky-300"><KeyRound :size="14" /></span>
                              <span class="apple-state-copy"><strong>OAuth2 授权</strong><small>访问令牌按需自动续期，refresh_token 加密存储</small></span>
                              <span :class="statusClass(selected)" class="apple-state-status">{{ statusLabel(selected.icloud_status || selected.status) }}</span>
                            </article>
                            <article class="apple-state-row">
                              <span class="apple-state-icon text-violet-600 bg-violet-50 dark:bg-violet-950/30 dark:text-violet-300"><Cloud :size="14" /></span>
                              <span class="apple-state-copy"><strong>Microsoft Graph</strong><small>读取收件箱并按收件人分发到各个别名</small></span>
                              <span class="apple-state-status bg-emerald-50 text-emerald-600 dark:bg-emerald-950/30 dark:text-emerald-300">已启用</span>
                            </article>
                          </div>
                          <div class="detail-field-grid">
                            <label class="form-group">
                              <span class="form-label">客户端 ID</span>
                              <input v-model.trim="authForm.client_id" class="field font-mono" type="text" autocomplete="off" maxlength="120" placeholder="留空使用内置公共客户端" />
                              <small class="form-help">留空则用内置的微软公共客户端 ID；长期使用建议填自己在 Azure 注册的应用。</small>
                            </label>
                            <label class="form-group">
                              <span class="form-label">租户</span>
                              <input v-model.trim="authForm.tenant" class="field font-mono" type="text" autocomplete="off" maxlength="120" placeholder="common" />
                              <small class="form-help">个人 Microsoft 账号（outlook.com / hotmail.com）保持 <code>common</code>。</small>
                            </label>
                            <label class="settings-capability-option">
                              <span class="settings-capability-option-icon"><Server :size="16" /></span>
                              <span class="settings-capability-option-copy"><strong>Graph 后台收信监听</strong><small>按收件人把 Graph 返回的整箱邮件分发到对应别名，默认 15 秒一轮，失败后退避。</small></span>
                              <input v-model="authForm.watcher_enabled" class="detail-switch" type="checkbox" />
                            </label>
                          </div>
                          <dl class="apple-meta-list">
                            <div><dt>别名上限</dt><dd>单账号 10 个（Microsoft 限制）</dd></div>
                          </dl>
                          <p v-if="moduleInfo.using_default_client" class="apple-detail-hint"><AlertCircle :size="13" />当前使用内置公共客户端，填入自己的 Azure 应用 ID 可提高稳定性。</p>
                          <div class="detail-section-foot">
                            <button type="button" class="secondary-button" :disabled="isBusy('save-auth')" @click="saveAuth"><LoaderCircle v-if="isBusy('save-auth')" :size="13" class="animate-spin" /><Save v-else :size="13" />{{ isBusy('save-auth') ? '保存中' : '保存授权信息' }}</button>
                          </div>
                        </div>
                      </section>

                      <!-- ② 邮件与验证码：默认折叠，内容由 AccountInboxPanel 自己渲染。 -->
                      <AccountInboxPanel collapsible :account-id="selected?.id || ''" :account-name="selected?.label || selected?.apple_id || ''" />

                      <!-- ③ 别名邮箱：列的是本机邮箱池，删掉就是连池子一起删。 -->
                      <section class="detail-section">
                        <header class="detail-section-head">
                          <button type="button" class="detail-section-toggle" :aria-expanded="openSections.aliases" @click="openSections.aliases = !openSections.aliases">
                            <ChevronRight :size="15" class="detail-section-caret" />
                            <span class="detail-section-title">
                              <strong><MailOpen :size="15" />别名邮箱</strong>
                              <small>{{ aliasMailboxes.length }} 个别名在邮箱池里 · Graph 不提供创建接口，请到 account.microsoft.com 手工创建后再导入</small>
                            </span>
                          </button>
                          <div class="detail-section-actions">
                            <button class="secondary-button apple-check-button" @click="openAddAliasDialog"><Plus :size="13" />手工添加</button>
                          </div>
                        </header>
                        <div v-if="openSections.aliases" class="detail-section-body">
                          <div v-if="!aliasMailboxes.length" class="apple-state-empty">这个账号还没有别名进邮箱池，点页面上的“发现别名”或“手工添加”。</div>
                          <ul v-else class="apple-alias-list">
                            <li v-for="mailbox in aliasMailboxes" :key="mailbox.id" class="apple-alias-row">
                              <MailOpen :size="13" />
                              <span class="font-mono">{{ mailbox.email }}</span>
                              <button type="button" class="apple-delete-button" :title="`删除 ${mailbox.email}`" :aria-label="`删除 ${mailbox.email}`" :disabled="isBusy(`alias-delete:${mailbox.id}`)" @click="deleteAlias(mailbox)"><LoaderCircle v-if="isBusy(`alias-delete:${mailbox.id}`)" :size="13" class="animate-spin" /><Trash2 v-else :size="13" /></button>
                            </li>
                          </ul>
                        </div>
                      </section>
                    </div>
                  </td>
                </tr>
                </template>
              </tbody>
            </table>
          </div>
        </section>
      </div>
    </section>

    <div v-if="showLogin" class="mailbox-dialog-backdrop" role="presentation" @click.self="closeLoginDialog">
      <section role="dialog" aria-modal="true" aria-labelledby="ms-login-title" class="panel mailbox-operation-dialog apple-login-dialog">
        <header class="mailbox-dialog-heading"><div><h2 id="ms-login-title"><Mail :size="17" />添加 Microsoft 账号</h2><p>使用设备码登录，密码只在 Microsoft 官方页面输入，本地只保存 OAuth2 令牌。</p></div><button class="icon-button" title="关闭" @click="closeLoginDialog"><X :size="16" /></button></header>

        <div v-if="!login.login_id" class="apple-dialog-fields">
          <p class="apple-dialog-intro">点击下面的按钮获取一个设备码，然后按提示在浏览器中完成登录。</p>
        </div>

        <div v-else class="apple-dialog-fields">
          <div class="ms-device-code">
            <span class="form-label">设备码</span>
            <div class="ms-device-code-row">
              <code>{{ login.user_code }}</code>
              <button type="button" class="secondary-button" @click="copyUserCode"><ClipboardCopy :size="13" />复制</button>
            </div>
          </div>
          <div class="ms-device-steps">
            <div><span>1</span><p>打开 <button type="button" class="link-button" @click="openVerificationPage">{{ login.verification_uri }}<ExternalLink :size="12" /></button></p></div>
            <div><span>2</span><p>输入上面的设备码并登录你要接入的 Microsoft 账号</p></div>
            <div><span>3</span><p>同意读取邮件的授权，页面会自动完成接入</p></div>
          </div>
          <p v-if="login.message" class="apple-dialog-intro">{{ login.message }}</p>
          <div class="ms-poll-status" :class="`ms-poll-${login.status}`">
            <LoaderCircle v-if="polling" :size="14" class="animate-spin" />
            <CheckCircle2 v-else-if="login.status === 'completed'" :size="14" />
            <AlertCircle v-else :size="14" />
            <span v-if="polling">等待浏览器中完成登录…<template v-if="remainingSeconds > 0">（剩余 {{ remainingSeconds }} 秒）</template></span>
            <span v-else-if="login.status === 'completed'">登录已完成</span>
            <span v-else-if="login.status === 'expired'">设备码已过期，请重新发起</span>
            <span v-else-if="login.status === 'failed'">登录失败，请查看提示后重试</span>
            <span v-else>准备就绪</span>
          </div>
        </div>

        <footer class="mailbox-dialog-actions">
          <button type="button" class="secondary-button" @click="closeLoginDialog">关闭</button>
          <button v-if="!login.login_id" class="primary-button" :disabled="isBusy('login-start')" @click="startLogin"><LoaderCircle v-if="isBusy('login-start')" :size="14" class="animate-spin" /><ShieldCheck v-else :size="14" />获取设备码</button>
          <button v-else class="primary-button" :disabled="!polling || isBusy('login-poll')" @click="pollLogin"><LoaderCircle v-if="isBusy('login-poll')" :size="14" class="animate-spin" /><RefreshCw v-else :size="14" />立即检查</button>
        </footer>
      </section>
    </div>

    <div v-if="showAddAlias" class="mailbox-dialog-backdrop" role="presentation" @click.self="showAddAlias = false">
      <form role="dialog" aria-modal="true" aria-labelledby="ms-alias-title" class="panel mailbox-operation-dialog apple-form-dialog" @submit.prevent="addAlias">
        <header class="mailbox-dialog-heading"><div><h2 id="ms-alias-title"><Plus :size="17" />手工添加别名</h2><p>当前账号：{{ selected?.label || selected?.apple_id }}</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('add-alias')" @click="showAddAlias = false"><X :size="16" /></button></header>
        <div class="apple-dialog-fields">
          <label class="form-group"><span class="form-label">别名邮箱地址</span><input v-model.trim="aliasForm.email" class="field font-mono" type="email" placeholder="alias@outlook.com" required /><span class="form-help">必须是该 Microsoft 账号上已经存在的别名。</span></label>
          <div class="apple-create-meta"><label class="form-group"><span class="form-label">标签</span><input v-model.trim="aliasForm.label" class="field" placeholder="可选" /></label><label class="form-group"><span class="form-label">备注</span><input v-model.trim="aliasForm.note" class="field" placeholder="可选备注" /></label></div>
        </div>
        <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('add-alias')" @click="showAddAlias = false">取消</button><button class="primary-button" :disabled="isBusy('add-alias')"><LoaderCircle v-if="isBusy('add-alias')" :size="14" class="animate-spin" /><Plus v-else :size="14" />添加别名</button></footer>
      </form>
    </div>
    <div v-if="showBatch" class="mailbox-dialog-backdrop" role="presentation" @click.self="showBatch = false">
      <form role="dialog" aria-modal="true" aria-labelledby="ms-batch-title" class="panel mailbox-operation-dialog apple-form-dialog" @submit.prevent="generateAliases">
        <header class="mailbox-dialog-heading"><div><h2 id="ms-batch-title"><Layers :size="17" />批量生成 + 地址</h2><p>当前账号：{{ selected?.label || selected?.apple_id }}</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('generate-batch')" @click="showBatch = false"><X :size="16" /></button></header>
        <div class="apple-dialog-fields">
          <label class="form-group"><span class="form-label">标签前缀（可选）</span><span class="apple-inline-field"><input v-model.trim="batchForm.prefix" class="field font-mono" placeholder="留空则纯随机" /><button type="button" class="secondary-button" :disabled="isBusy('generate-batch')" @click="batchForm.prefix = randomPrefix()">随机</button></span><span class="form-help">填 shop 会生成形如 {{ batchPreview }} 的地址。</span></label>
          <label class="form-group"><span class="form-label">创建数量</span><input v-model.number="batchForm.count" class="field" type="number" min="1" :max="BATCH_MAX" required /><span class="form-help">单次最多 {{ BATCH_MAX }} 个。</span></label>
          <div class="apple-create-meta"><label class="form-group"><span class="form-label">标签</span><input v-model.trim="batchForm.label" class="field" placeholder="留空则用地址本身" /></label><label class="form-group"><span class="form-label">备注</span><input v-model.trim="batchForm.note" class="field" placeholder="可选备注" /></label></div>
          <p class="apple-dialog-intro">随机后缀由服务端生成，生成前会和邮箱池里已有的全部地址比对，<strong>所以下次再建多少都不会和这次的重复</strong>。免费 Outlook.com 支持 + 标签寻址：邮件投递到主地址的同一个收件箱，收件人字段会保留标签（已实测确认）。地址必须先在这里建好才能收到信——路由是按收件人精确匹配的。</p>
        </div>
        <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('generate-batch')" @click="showBatch = false">取消</button><button class="primary-button" :disabled="isBusy('generate-batch')"><LoaderCircle v-if="isBusy('generate-batch')" :size="14" class="animate-spin" /><Layers v-else :size="14" />生成 {{ batchForm.count || 0 }} 个地址</button></footer>
      </form>
    </div>
  </div>
</template>
