<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { AlertCircle, ChevronRight, Globe, Inbox, KeyRound, Layers, LoaderCircle, Mail, MailOpen, Plus, RefreshCw, Save, Search, Server, Tag, Trash2, X } from '@lucide/vue'
import { api } from '../api/client'
import { useConfirm } from '../composables/useConfirm'
import { subscribeRealtime } from '../composables/useRealtime'
import { useToast } from '../composables/useToast'
import AccountInboxPanel from '../components/AccountInboxPanel.vue'
import ListPager from '../components/ListPager.vue'

const route = useRoute()
const router = useRouter()

const loading = ref(true)
const busyActions = ref([])
const showGenerate = ref(false)
const showAddMailbox = ref(false)
const data = ref({ items: [], module: {}, module_ready: true })
const selected = ref(null)
// 列表分页：账号会越加越多，一次全渲染到页面上既慢又重，所以只取当前这一页。
// 搜索和分页一起存在才有用：只有分页时找一条记录要点几十次「下一页」，
// 只有搜索时一次匹配几百条又会把体积问题原样搬回来。
const page = ref(1)
const query = ref('')
const total = ref(0)
const totalPages = ref(1)
let searchTimer
const session = ref({})
const aliases = ref([])
const aliasNotice = ref('')
const redirectURI = ref('')
const aliasForm = reactive({ email: '', label: '', note: '' })
const tagForm = reactive({ tag: '', label: '', note: '' })

// 应用凭据（客户端 ID / 密钥 / 代理）在这里改：系统设置页不再重复展示授权信息。
// 它必须放在账号列表之上——一个账号都还没建的时候，就得先能填这里。
const credentialsOpen = ref(false)
const credentialsForm = reactive({ client_id: '', client_secret: '', proxy: '', watcher_enabled: true })
const googleStatus = ref({})
let credentialsLoaded = false

// 批量生成用独立的表单：前缀留空即纯随机，数量决定一次建多少个。
const showBatch = ref(false)
const batchForm = reactive({ prefix: '', count: 100, label: '', note: '' })
const BATCH_MAX = 1000
// 「随机」按钮用的字符集，和后端保持一致。真正的随机后缀是服务端生成的，
// 这里只决定前缀建议长什么样，所以两边不同步也不影响地址的正确性。
const PLUS_TAG_ALPHABET = 'abcdefghijklmnopqrstuvwxyz0123456789'

// 给前缀填一个随机值，让使用者不必自己想。
function randomPrefix() {
  const values = new Uint32Array(6)
  crypto.getRandomValues(values)
  return [...values].map((value) => PLUS_TAG_ALPHABET[value % PLUS_TAG_ALPHABET.length]).join('')
}

const batchPreview = computed(() => {
  const base = (selected.value?.apple_id || 'you@gmail.com').split('@')[0].split('+')[0]
  const prefix = (batchForm.prefix || '').trim().toLowerCase()
  const sample = prefix ? `${prefix}-xxxxxxxx` : 'xxxxxxxx'
  return `${base}+${sample}@gmail.com`
})
const { success, error: showError } = useToast()
const { confirm: confirmAction } = useConfirm()
let realtimeRefreshTimer
let realtimeUnsubscribe = () => {}

const moduleReady = computed(() => data.value.module_ready !== false)
const moduleInfo = computed(() => data.value.module || {})
const moduleConfigured = computed(() => moduleInfo.value.configured !== false)
const loginTitle = computed(() => {
  if (!moduleReady.value) return 'Google 模块已在配置中关闭'
  if (!moduleConfigured.value) return '请先在上方「应用授权信息」里填写 Google 客户端 ID'
  return '跳转到 Google 授权页面，用浏览器登录'
})

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

function accountName(account) {
  return account.label || account.apple_id || account.id
}

async function load(options = {}) {
  const silent = Boolean(options.silent)
  if (!silent) loading.value = true
  try {
    const params = new URLSearchParams({ page: String(page.value) })
    if (query.value.trim()) params.set('q', query.value.trim())
    const payload = await api(`/api/google-accounts?${params.toString()}`)
    data.value = payload
    total.value = payload.total || 0
    totalPages.value = payload.total_pages || 1
    page.value = payload.page || 1
    await loadGoogleStatus()
    if (selected.value) {
      const current = (payload.items || []).find((item) => item.id === selected.value.id)
      if (current) selected.value = { ...selected.value, ...current }
      // 翻页或搜索之后选中项可能已经不在当前页上。不清掉的话右侧会显示一个列表里
      // 根本看不到的账号，看起来像它被删了。
      else {
        selected.value = null
        session.value = {}
        aliases.value = []
      }
    }
  } catch (err) {
    flash(err.message, true)
  } finally {
    if (!silent) loading.value = false
  }
}

function changePage(target) {
  page.value = target
  load()
}

// 凭据表单只在首次加载和保存之后回填。load() 会被实时事件触发，每次都回填
// 会把使用者正在输入的内容冲掉。
function applyCredentials(force = false) {
  if (credentialsLoaded && !force) return
  const status = googleStatus.value || {}
  credentialsForm.client_id = String(status.client_id || '')
  credentialsForm.client_secret = ''
  credentialsForm.proxy = String(status.proxy || '')
  credentialsForm.watcher_enabled = status.watcher_enabled !== false
  credentialsLoaded = true
  // 还没配好客户端就直接展开：不填这里，一个 Google 账号都加不了。
  if (!status.configured) credentialsOpen.value = true
}

async function loadGoogleStatus() {
  try {
    googleStatus.value = await api('/api/google/status')
  } catch {
    // Google 模块不可用时不该拖垮整个页面。
    googleStatus.value = {}
  }
  applyCredentials()
}

async function saveCredentials() {
  if (!startBusy('save-credentials')) return
  try {
    googleStatus.value = await api('/api/google/settings', {
      method: 'PUT',
      body: JSON.stringify({
        client_id: credentialsForm.client_id,
        client_secret: credentialsForm.client_secret,
        proxy: credentialsForm.proxy,
        watcher_enabled: credentialsForm.watcher_enabled,
      }),
    })
    applyCredentials(true)
    credentialsOpen.value = !googleStatus.value.configured
    flash('Google 授权信息已保存')
    await load({ silent: true })
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('save-credentials')
  }
}

function scheduleRealtimeRefresh(change) {
  if (change.resource === 'google-session') {
    window.clearTimeout(realtimeRefreshTimer)
    realtimeRefreshTimer = window.setTimeout(() => load({ silent: true }), 120)
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
  realtimeRefreshTimer = window.setTimeout(() => load({ silent: true }), 120)
}

// Google 的授权是「浏览器跳出去、Google 回调跳回来」，回调结果以 query 的形式落在本页。
// 提示完立刻把 query 清掉，避免用户刷新页面时重复弹同一条提示。
async function consumeCallbackQuery() {
  const status = String(route.query.google || '').trim()
  if (!status) return
  const message = String(route.query.message || '').trim()
  const query = { ...route.query }
  delete query.google
  delete query.message
  router.replace({ path: route.path, query })
  if (status === 'connected') {
    flash(message ? `Google 账号已连接：${message}` : 'Google 账号已连接')
    return
  }
  flash(message || 'Google 授权未完成，请重新发起', true)
}

async function startLogin() {
  if (!startBusy('login-start')) return
  try {
    const result = await api('/api/google-accounts/login/start', { method: 'POST' })
    const authUrl = String(result.auth_url || '')
    if (!authUrl) {
      flash('服务端没有返回授权链接，请检查 Google 客户端配置', true)
      return
    }
    redirectURI.value = result.redirect_uri || ''
    flash('正在跳转到 Google 授权页面…')
    window.location.href = authUrl
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('login-start')
  }
}

async function selectAccount(account) {
  const busyKey = `detail:${account.id}`
  if (!startBusy(busyKey)) return
  try {
    const result = await api(`/api/google-accounts/${account.id}`)
    selected.value = result.account
    session.value = result.session || {}
    aliases.value = (result.mailboxes || []).map((item) => item.email)
    aliasNotice.value = ''
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy(busyKey)
  }
}

async function deleteAccount(account) {
  const name = accountName(account)
  const confirmed = await confirmAction({
    title: '删除 Google 账号',
    message: `确定删除“${name}”吗？\n\n本地 OAuth2 授权、关联地址和本地邮件会一并删除；Google 账号本身不受影响。`,
    confirmText: '删除账号',
    tone: 'danger',
  })
  if (!confirmed) return
  const busyKey = `delete:${account.id}`
  if (!startBusy(busyKey)) return
  try {
    const result = await api(`/api/google-accounts/${account.id}`, { method: 'DELETE' })
    if (selected.value?.id === account.id) {
      selected.value = null
      session.value = {}
      aliases.value = []
      aliasNotice.value = ''
    }
    await load()
    const deleted = result.deleted || {}
    flash(`已删除 Google 账号：${name}；清理地址 ${deleted.mailboxes || 0} 个，邮件 ${deleted.messages || 0} 封`)
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
    const result = await api(`/api/google-accounts/${selected.value.id}/check`, { method: 'POST' })
    if (result.session) session.value = result.session
    flash('Google 授权校验通过')
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
    const result = await api(`/api/google-accounts/${selected.value.id}/aliases`)
    aliases.value = result.aliases || []
    flash(result.count ? `读取到 ${result.count} 个可收信地址` : '该账号没有返回可收信地址，请先用「生成 + 地址」创建一个')
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
    const result = await api(`/api/google-accounts/${selected.value.id}/aliases/import`, { method: 'POST' })
    aliases.value = result.aliases || []
    aliasNotice.value = result.notice || ''
    flash(`主地址导入完成：新增 ${result.created} 个，更新 ${result.updated} 个`)
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('import')
  }
}

function openGenerateDialog() {
  if (!selected.value) {
    flash('请先选择一个 Google 账号', true)
    return
  }
  tagForm.tag = ''
  tagForm.label = ''
  tagForm.note = ''
  showGenerate.value = true
}

async function generateAlias() {
  if (!selected.value) return
  if (!startBusy('generate')) return
  try {
    const result = await api(`/api/google-accounts/${selected.value.id}/aliases/generate`, {
      method: 'POST',
      body: JSON.stringify(tagForm),
    })
    showGenerate.value = false
    flash(`${result.created ? '已生成' : '已更新'}地址：${result.mailbox.email}`)
    aliases.value = [...new Set([...aliases.value, result.mailbox.email])]
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('generate')
  }
}

function openBatchDialog() {
  if (!selected.value) {
    flash('请先选择一个 Google 账号', true)
    return
  }
  batchForm.prefix = ''
  batchForm.count = 100
  batchForm.label = ''
  batchForm.note = ''
  showBatch.value = true
}

// 批量生成：前缀留空就纯随机，后端保证跨批次不会重复。
async function generateAliases() {
  if (!selected.value) return
  const count = Number(batchForm.count)
  if (!Number.isInteger(count) || count < 1 || count > BATCH_MAX) {
    flash(`数量必须是 1 到 ${BATCH_MAX} 之间的整数`, true)
    return
  }
  if (!startBusy('generate-batch')) return
  try {
    const result = await api(`/api/google-accounts/${selected.value.id}/aliases/generate-batch`, {
      method: 'POST',
      body: JSON.stringify({ ...batchForm, count }),
    })
    showBatch.value = false
    const skipped = result.skipped ? `，未能生成 ${result.skipped} 个` : ''
    flash(`已生成 ${result.created} 个 + 地址${skipped}`)
    aliases.value = [...new Set([...aliases.value, ...(result.addresses || [])])]
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('generate-batch')
  }
}

function openAddMailboxDialog() {
  if (!selected.value) {
    flash('请先选择一个 Google 账号', true)
    return
  }
  aliasForm.email = ''
  aliasForm.label = ''
  aliasForm.note = ''
  showAddMailbox.value = true
}

async function addMailbox() {
  if (!selected.value) return
  if (!startBusy('add-mailbox')) return
  try {
    const result = await api(`/api/google-accounts/${selected.value.id}/mailboxes`, {
      method: 'POST',
      body: JSON.stringify(aliasForm),
    })
    showAddMailbox.value = false
    flash(`${result.created ? '已添加' : '已更新'}地址：${result.mailbox.email}`)
    aliases.value = [...new Set([...aliases.value, result.mailbox.email])]
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('add-mailbox')
  }
}

// 搜索防抖：每敲一个字就发一次请求既浪费，也会让列表在打字时不停闪。
// 搜索时回到第一页——停在第五页搜一个只有两条结果的词，会看到空列表。
watch(query, () => {
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => {
    page.value = 1
    load()
  }, 300)
})

onMounted(async () => {
  await load()
  await consumeCallbackQuery()
  realtimeUnsubscribe = subscribeRealtime(['google-session', 'apple-account', 'mailbox'], scheduleRealtimeRefresh)
})

onBeforeUnmount(() => {
  window.clearTimeout(realtimeRefreshTimer)
  window.clearTimeout(searchTimer)
  realtimeUnsubscribe()
})
</script>

<template>
  <div class="apple-account-page">
    <section class="panel apple-account-workbench">
      <header class="apple-command-bar">
        <div class="apple-command-title"><span><Globe :size="16" /></span><div><h2>Google 账号</h2><p>通过 Gmail API 读取 + 标签地址的验证码</p></div></div>
        <div class="apple-command-actions">
          <button class="primary-button apple-command-button" :disabled="!moduleReady || !moduleConfigured" :title="loginTitle" @click="startLogin"><LoaderCircle v-if="isBusy('login-start')" :size="14" class="animate-spin" /><Plus v-else :size="14" />添加 Google 账号</button>
          <button class="secondary-button apple-command-button" :disabled="!selected || isBusy('aliases')" :title="selected ? '读取该账号当前可收信的地址' : '请先选择一个 Google 账号'" @click="discoverAliases"><LoaderCircle v-if="isBusy('aliases')" :size="14" class="animate-spin" /><RefreshCw v-else :size="14" />读取地址</button>
          <button class="secondary-button apple-command-button" :disabled="!selected || isBusy('import')" :title="selected ? '把主地址写入邮箱池' : '请先选择一个 Google 账号'" @click="importAliases"><LoaderCircle v-if="isBusy('import')" :size="14" class="animate-spin" /><Inbox v-else :size="14" />导入主地址</button>
          <button class="secondary-button apple-command-button" :disabled="!selected || isBusy('generate')" :title="selected ? '用 + 标签生成一个新地址' : '请先选择一个 Google 账号'" @click="openGenerateDialog"><Tag :size="14" />生成 + 地址</button>
          <button class="secondary-button apple-command-button" :disabled="!selected || isBusy('generate-batch')" :title="selected ? '随机批量生成 + 地址，跨批次不会重复' : '请先选择一个 Google 账号'" @click="openBatchDialog"><LoaderCircle v-if="isBusy('generate-batch')" :size="14" class="animate-spin" /><Layers v-else :size="14" />批量生成</button>
        </div>
      </header>

      <div v-if="moduleInfo.alias_limit_note" class="apple-module-note">
        <AlertCircle :size="14" />
        <span>{{ moduleInfo.alias_limit_note }}</span>
      </div>

      <!-- Google 的客户端凭据在这里改：系统设置页不再重复展示授权信息。
           放在账号列表之上，是因为一个账号都还没建的时候就必须先能填。 -->
      <section class="detail-section google-credentials">
        <header class="detail-section-head">
          <button type="button" class="detail-section-toggle" :aria-expanded="credentialsOpen" @click="credentialsOpen = !credentialsOpen">
            <ChevronRight :size="15" class="detail-section-caret" />
            <span class="detail-section-title">
              <strong><KeyRound :size="15" />应用授权信息</strong>
              <small>{{ moduleConfigured ? '已配置客户端，点击可修改' : '还没配置客户端，不填这里一个 Google 账号都加不了' }}</small>
            </span>
          </button>
        </header>
        <div v-if="credentialsOpen" class="detail-section-body">
          <p v-if="!moduleConfigured" class="apple-detail-hint">
            <AlertCircle :size="13" />
            <span>{{ moduleInfo.setup_note || 'Google 没有可以内置的公共客户端，必须自己在 Google Cloud Console 建一个「桌面应用」类型的 OAuth 客户端，启用 Gmail API，并把自己的 Gmail 加为测试用户。' }}</span>
          </p>
          <div class="detail-field-grid">
            <label class="form-group">
              <span class="form-label">客户端 ID（必填）</span>
              <input v-model.trim="credentialsForm.client_id" class="field font-mono" type="text" autocomplete="off" maxlength="200" placeholder="xxxxxxxx.apps.googleusercontent.com" />
              <small class="form-help">Google Cloud Console → 凭据 → 创建 OAuth 客户端 ID → 应用类型选「<strong>桌面应用</strong>」。</small>
            </label>
            <label class="form-group">
              <span class="form-label">客户端密钥（必填）</span>
              <input v-model.trim="credentialsForm.client_secret" class="field font-mono" type="password" autocomplete="off" maxlength="200" :placeholder="googleStatus.has_client_secret ? '已保存，留空则不变更' : '粘贴凭据页的客户端密钥'" />
              <small class="form-help">Google 文档把它标为「可选」，但实测换取令牌时<strong>必须提供</strong>，桌面应用类型也一样。</small>
            </label>
            <label class="form-group">
              <span class="form-label">代理地址</span>
              <input v-model.trim="credentialsForm.proxy" class="field font-mono" type="text" autocomplete="off" maxlength="200" :placeholder="googleStatus.proxy_detected ? '已自动检测到系统代理' : '例如 http://127.0.0.1:7897'" />
              <small class="form-help">留空则自动读取系统代理。<strong>Go 不会自己读 Windows 的系统代理设置</strong>，Google 在部分网络里无法直连。</small>
            </label>
            <label class="settings-capability-option">
              <span class="settings-capability-option-icon"><Globe :size="16" /></span>
              <span class="settings-capability-option-copy"><strong>Gmail 后台收信监听</strong><small>按 Delivered-To / To 把收件箱分发到各 + 标签地址，默认 15 秒一轮，失败后退避。</small></span>
              <input v-model="credentialsForm.watcher_enabled" class="detail-switch" type="checkbox" />
            </label>
          </div>
          <div class="detail-section-foot">
            <button type="button" class="secondary-button" :disabled="isBusy('save-credentials')" @click="saveCredentials"><LoaderCircle v-if="isBusy('save-credentials')" :size="14" class="animate-spin" /><Save v-else :size="14" />{{ isBusy('save-credentials') ? '保存中' : '保存授权信息' }}</button>
          </div>
        </div>
      </section>

      <div v-if="loading" class="apple-loading"><LoaderCircle :size="16" class="animate-spin" />正在加载 Google 账号</div>
      <div v-else class="apple-account-body">
        <section class="apple-account-list">
          <header class="apple-section-heading"><div><h3>账号与授权</h3><p>选择账号后可在右侧查看 OAuth2 授权与地址</p></div><label class="field-wrap mailbox-search"><Search :size="15" class="field-icon" /><input v-model="query" class="field field-leading" type="search" placeholder="搜索账号或标签" aria-label="搜索账号或标签" /></label><span>总 {{ total }} 个账号</span></header>
          <div v-if="!data.items?.length" class="apple-empty"><span><Server :size="20" /></span><strong>{{ query.trim() ? '没有匹配的 Google 账号' : '还没有 Google 账号' }}</strong><small>{{ query.trim() ? '换个关键词试试，或清空搜索框。' : '点击“添加 Google 账号”，在浏览器中完成 Google 授权。' }}</small></div>
          <div v-else class="apple-account-table-viewport">
            <table class="apple-account-table">
              <colgroup><col class="apple-col-account" /><col class="apple-col-status" /><col class="apple-col-channel" /><col class="apple-col-action" /></colgroup>
              <thead><tr><th>Google 账号</th><th>状态</th><th>读信通道</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="account in data.items" :key="account.id" class="apple-account-row" :class="{ 'apple-account-row-selected': selected?.id === account.id }" tabindex="0" :aria-label="`查看 ${accountName(account)} 的授权详情`" @click="selectAccount(account)" @keydown.enter="selectAccount(account)" @keydown.space.prevent="selectAccount(account)">
                  <td><button type="button" class="apple-account-select" :disabled="isBusy(`delete:${account.id}`) || isBusy(`detail:${account.id}`)" @click.stop="selectAccount(account)"><span class="apple-account-icon"><LoaderCircle v-if="isBusy(`detail:${account.id}`)" :size="14" class="animate-spin" /><Mail v-else :size="14" /></span><span><strong>{{ accountName(account) }}</strong><small>{{ account.apple_id || account.id }}</small></span></button></td>
                  <td><span :class="statusClass(account)" class="apple-status-badge">{{ statusLabel(account.icloud_status || account.status) }}</span></td>
                  <td><div class="apple-channel-list"><span class="apple-channel-pill"><Globe :size="11" /><span>Gmail API</span><em class="bg-emerald-50 text-emerald-600 dark:bg-emerald-950/30 dark:text-emerald-300">官方 API</em></span></div></td>
                  <td><button type="button" class="apple-delete-button" :title="`删除 ${accountName(account)}`" :aria-label="`删除 ${accountName(account)}`" :disabled="isBusy(`delete:${account.id}`)" @click.stop="deleteAccount(account)"><LoaderCircle v-if="isBusy(`delete:${account.id}`)" :size="13" class="animate-spin" /><Trash2 v-else :size="13" /></button></td>
                </tr>
              </tbody>
            </table>
          </div>
          <ListPager :page="page" :total-pages="totalPages" :total="total" unit="个账号" :loading="loading" @change="changePage" />
        </section>

        <aside class="apple-detail-panel">
          <div v-if="!selected" class="apple-detail-empty"><span><Globe :size="20" /></span><strong>选择一个 Google 账号</strong><small>OAuth2 授权状态与地址列表会显示在这里。</small></div>
          <template v-else>
            <header class="apple-detail-heading"><div><span>所选 Google 账号</span><h3>{{ selected.label || selected.apple_id }}</h3><p>{{ selected.apple_id }}</p></div><button class="secondary-button apple-check-button" :disabled="isBusy('check')" @click="checkAccount"><LoaderCircle v-if="isBusy('check')" :size="13" class="animate-spin" /><RefreshCw v-else :size="13" />{{ isBusy('check') ? '检查中' : '校验授权' }}</button></header>
            <div class="ms-detail-scroll">
            <div class="apple-state-list">
              <article class="apple-state-row">
                <span class="apple-state-icon text-sky-600 bg-sky-50 dark:bg-sky-950/30 dark:text-sky-300"><KeyRound :size="14" /></span>
                <span class="apple-state-copy"><strong>OAuth2 授权</strong><small>访问令牌按需自动续期，refresh_token 加密存储</small></span>
                <span :class="statusClass(selected)" class="apple-state-status">{{ statusLabel(selected.icloud_status || selected.status) }}</span>
              </article>
              <article class="apple-state-row">
                <span class="apple-state-icon text-violet-600 bg-violet-50 dark:bg-violet-950/30 dark:text-violet-300"><Globe :size="14" /></span>
                <span class="apple-state-copy"><strong>Gmail API</strong><small>读取收件箱并按收件人分发到各个地址</small></span>
                <span class="apple-state-status bg-emerald-50 text-emerald-600 dark:bg-emerald-950/30 dark:text-emerald-300">已启用</span>
              </article>
            </div>

            <div class="apple-detail-block">
              <header class="apple-section-heading"><div><h3>收信地址</h3><p>免费 Gmail 无法创建附加地址，只能用 + 标签生成</p></div><button class="secondary-button apple-check-button" @click="openAddMailboxDialog"><Plus :size="13" />手工添加</button></header>
              <p v-if="aliasNotice" class="apple-detail-hint"><AlertCircle :size="13" /><span>{{ aliasNotice }}</span></p>
              <div v-if="!aliases.length" class="apple-state-empty">还没有已导入的地址，点击“读取地址”或“生成 + 地址”。</div>
              <ul v-else class="apple-alias-list">
                <li v-for="alias in aliases" :key="alias"><MailOpen :size="13" /><span class="font-mono">{{ alias }}</span></li>
              </ul>
            </div>

            <div class="apple-detail-block">
              <header class="apple-section-heading"><div><h3>授权信息</h3><p>客户端与代理来自系统设置或 config.json</p></div></header>
              <dl class="apple-meta-list">
                <div><dt>客户端 ID</dt><dd class="font-mono">{{ moduleInfo.client_id || '—' }}</dd></div>
                <div><dt>回调地址</dt><dd class="font-mono">{{ redirectURI || moduleInfo.redirect_uri || '—' }}</dd></div>
                <div><dt>授权范围</dt><dd class="font-mono">{{ session.scope || '—' }}</dd></div>
                <div><dt>代理</dt><dd class="font-mono">{{ moduleInfo.proxy_detected ? (moduleInfo.proxy || '已启用') : '未配置' }}</dd></div>
                <div><dt>后台监听</dt><dd>{{ moduleInfo.watcher_enabled ? '已开启' : '已关闭' }}</dd></div>
                <div><dt>上次校验</dt><dd>{{ formatTime(session.last_checked_at) }}</dd></div>
              </dl>
              <p v-if="!session.exists" class="apple-detail-hint"><AlertCircle :size="13" /><span>本地没有这个账号的 Google 登录态，请重新授权后再试。</span></p>
              <p v-else-if="session.last_status_message" class="apple-detail-hint"><AlertCircle :size="13" /><span>{{ session.last_status_message }}</span></p>
            </div>
            </div>
          </template>
          <AccountInboxPanel :account-id="selected?.id || ''" :account-name="selected?.label || selected?.apple_id || ''" />
        </aside>
      </div>
    </section>

    <div v-if="showGenerate" class="mailbox-dialog-backdrop" role="presentation" @click.self="showGenerate = false">
      <form role="dialog" aria-modal="true" aria-labelledby="google-generate-title" class="panel mailbox-operation-dialog apple-form-dialog" @submit.prevent="generateAlias">
        <header class="mailbox-dialog-heading"><div><h2 id="google-generate-title"><Tag :size="17" />生成 + 标签地址</h2><p>当前账号：{{ selected?.label || selected?.apple_id }}</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('generate')" @click="showGenerate = false"><X :size="16" /></button></header>
        <div class="apple-dialog-fields">
          <label class="form-group"><span class="form-label">+ 标签</span><input v-model.trim="tagForm.tag" class="field font-mono" placeholder="shop" required /><span class="form-help">填 shop 会生成 you+shop@gmail.com；+ 标签不能包含 @ 或空白字符。</span></label>
          <div class="apple-create-meta"><label class="form-group"><span class="form-label">显示名称</span><input v-model.trim="tagForm.label" class="field" placeholder="可选" /></label><label class="form-group"><span class="form-label">备注</span><input v-model.trim="tagForm.note" class="field" placeholder="可选备注" /></label></div>
          <p class="apple-dialog-intro">+ 地址与主地址共用一个收件箱，会暴露真实地址，部分网站会拒绝或截断 + 之后的字符。</p>
        </div>
        <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('generate')" @click="showGenerate = false">取消</button><button class="primary-button" :disabled="isBusy('generate')"><LoaderCircle v-if="isBusy('generate')" :size="14" class="animate-spin" /><Tag v-else :size="14" />生成地址</button></footer>
      </form>
    </div>

    <div v-if="showBatch" class="mailbox-dialog-backdrop" role="presentation" @click.self="showBatch = false">
      <form role="dialog" aria-modal="true" aria-labelledby="google-batch-title" class="panel mailbox-operation-dialog apple-form-dialog" @submit.prevent="generateAliases">
        <header class="mailbox-dialog-heading"><div><h2 id="google-batch-title"><Layers :size="17" />批量生成 + 地址</h2><p>当前账号：{{ selected?.label || selected?.apple_id }}</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('generate-batch')" @click="showBatch = false"><X :size="16" /></button></header>
        <div class="apple-dialog-fields">
          <div class="apple-create-meta">
            <label class="form-group"><span class="form-label">标签前缀（可选）</span><span class="apple-inline-field"><input v-model.trim="batchForm.prefix" class="field font-mono" placeholder="留空则纯随机" /><button type="button" class="secondary-button" :disabled="isBusy('generate-batch')" @click="batchForm.prefix = randomPrefix()">随机</button></span><span class="form-help">填 shop 会生成形如 {{ batchPreview }} 的地址。</span></label>
            <label class="form-group"><span class="form-label">创建数量</span><input v-model.number="batchForm.count" class="field" type="number" min="1" :max="BATCH_MAX" step="1" required /><span class="form-help">1 到 {{ BATCH_MAX }} 个。</span></label>
          </div>
          <div class="apple-create-meta"><label class="form-group"><span class="form-label">显示名称</span><input v-model.trim="batchForm.label" class="field" placeholder="可选" /></label><label class="form-group"><span class="form-label">备注</span><input v-model.trim="batchForm.note" class="field" placeholder="可选备注" /></label></div>
          <p class="apple-dialog-intro">随机后缀由服务端生成，生成前会和邮箱池里已有的全部地址比对，<strong>所以下次再建多少都不会和这次的重复</strong>。+ 地址与主地址共用一个收件箱，会暴露真实地址，部分网站会拒绝或截断 + 之后的字符。</p>
        </div>
        <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('generate-batch')" @click="showBatch = false">取消</button><button class="primary-button" :disabled="isBusy('generate-batch')"><LoaderCircle v-if="isBusy('generate-batch')" :size="14" class="animate-spin" /><Layers v-else :size="14" />生成 {{ batchForm.count || 0 }} 个地址</button></footer>
      </form>
    </div>

    <div v-if="showAddMailbox" class="mailbox-dialog-backdrop" role="presentation" @click.self="showAddMailbox = false">
      <form role="dialog" aria-modal="true" aria-labelledby="google-mailbox-title" class="panel mailbox-operation-dialog apple-form-dialog" @submit.prevent="addMailbox">
        <header class="mailbox-dialog-heading"><div><h2 id="google-mailbox-title"><Plus :size="17" />手工添加地址</h2><p>当前账号：{{ selected?.label || selected?.apple_id }}</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('add-mailbox')" @click="showAddMailbox = false"><X :size="16" /></button></header>
        <div class="apple-dialog-fields">
          <label class="form-group"><span class="form-label">邮箱地址</span><input v-model.trim="aliasForm.email" class="field font-mono" type="email" placeholder="you@gmail.com" required /><span class="form-help">用于这个账号上已有的其他可收信 Gmail 地址。</span></label>
          <div class="apple-create-meta"><label class="form-group"><span class="form-label">显示名称</span><input v-model.trim="aliasForm.label" class="field" placeholder="可选" /></label><label class="form-group"><span class="form-label">备注</span><input v-model.trim="aliasForm.note" class="field" placeholder="可选备注" /></label></div>
        </div>
        <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('add-mailbox')" @click="showAddMailbox = false">取消</button><button class="primary-button" :disabled="isBusy('add-mailbox')"><LoaderCircle v-if="isBusy('add-mailbox')" :size="14" class="animate-spin" /><Plus v-else :size="14" />添加地址</button></footer>
      </form>
    </div>
  </div>
</template>
