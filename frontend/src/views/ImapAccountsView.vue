<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { AlertCircle, CheckCircle2, ChevronRight, FileUp, Inbox, KeyRound, LoaderCircle, MailOpen, Plus, RefreshCw, Search, Server, Trash2, X } from '@lucide/vue'
import { api } from '../api/client'
import { useConfirm } from '../composables/useConfirm'
import { subscribeRealtime } from '../composables/useRealtime'
import { useToast } from '../composables/useToast'
import AccountInboxPanel from '../components/AccountInboxPanel.vue'
import ListPager from '../components/ListPager.vue'

// QQ 和 163 共用这一个组件，靠路由上的 preset 分开。
// 两者协议完全相同，只是服务器地址不同，所以复制一份页面没有意义。
const route = useRoute()
const preset = computed(() => String(route.meta.preset || 'qq'))
const presetName = computed(() => String(route.meta.presetName || 'IMAP 邮箱'))
// 按钮上不带空格：使用者说的是「添加QQ邮箱」，不是「添加 QQ 邮箱」。
const presetLabel = computed(() => String(route.meta.presetLabel || presetName.value))

const loading = ref(true)
const busyActions = ref([])
const showCreate = ref(false)
const showAddAddress = ref(false)
const data = ref({ items: [], module: {} })
const selected = ref(null)
// 展开块里的分区默认都折叠，和其余服务商页面保持同一套手感。
const openSections = reactive({ secret: false, addresses: false, provider: false })
// 列表分页：几百个母号一次全渲染既慢又重，所以只取当前这一页。
// 搜索和分页必须一起存在——只有分页时，在几百行里找一条要点几十次「下一页」；
// 只有搜索时，一次匹配到几百条又把体积问题原样搬回来。
const page = ref(1)
const query = ref('')
const total = ref(0)
const totalPages = ref(1)
let searchTimer
const createForm = reactive({ email: '', secret: '', host: '', port: 0, username: '', label: '', note: '', raw: '' })
const addressForm = reactive({ raw: '' })
// 批量添加：一次建多个彼此独立的母号，每行「邮箱:授权码」。
// 这与 createForm 里的「名下其它地址」是两件事——那里的多个地址共用一个授权码。
const showBatch = ref(false)
const batchForm = reactive({ raw: '', host: '', port: 0 })
const batchResult = ref(null)
const batchFileInput = ref(null)
const { success, error: showError } = useToast()
const { confirm: confirmAction } = useConfirm()
let realtimeRefreshTimer
let realtimeUnsubscribe = () => {}

const moduleInfo = computed(() => data.value.module || {})
const accounts = computed(() => data.value.items || [])

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
  return ({ active: '正常', partial: '部分地址失败', failed: '同步失败', need_login: '需要授权码' })[value] || value || '未知'
}

function statusClass(account) {
  const status = account.icloud_status || account.status
  if (status === 'active') return 'bg-emerald-50 text-emerald-600 dark:bg-emerald-950/30 dark:text-emerald-300'
  if (status === 'partial') return 'bg-amber-50 text-amber-600 dark:bg-amber-950/30 dark:text-amber-300'
  return 'bg-rose-50 text-rose-600 dark:bg-rose-950/30 dark:text-rose-300'
}

// 与后端 ParseIMAPMailboxLines 保持同一套规则：空行和 # 注释跳过，
// 逗号、分号、制表符、空格分隔的一行多个地址都认，重复只留一条。
function parseAddresses(raw) {
  const seen = new Set()
  const out = []
  for (const line of String(raw || '').replace(/\r\n/g, '\n').split('\n')) {
    const candidate = line.trim()
    if (!candidate || candidate.startsWith('#')) continue
    for (const piece of candidate.split(/[\s,;]+/)) {
      const email = piece.trim().toLowerCase()
      if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) continue
      if (seen.has(email)) continue
      seen.add(email)
      out.push(email)
    }
  }
  return out
}

// 与后端 ParseIMAPCredentialLines 保持同一套规则：只在第一个冒号或制表符处切开，
// 空行和 # 注释跳过，同一邮箱只留第一条。
function parseCredentialLines(raw) {
  const seen = new Set()
  const items = []
  let skipped = 0
  for (const line of String(raw || '').replace(/\r\n/g, '\n').split('\n')) {
    const candidate = line.trim()
    if (!candidate || candidate.startsWith('#')) continue
    const index = candidate.search(/[:\t]/)
    if (index <= 0) {
      skipped++
      continue
    }
    const email = candidate.slice(0, index).trim().toLowerCase()
    const secret = compactAppPassword(candidate.slice(index + 1))
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email) || !secret) {
      skipped++
      continue
    }
    if (seen.has(email)) continue
    seen.add(email)
    items.push({ email, secret })
  }
  return { items, skipped }
}

// 只在「去掉空白后正好是 16 位字母数字」时去掉空白：那正是 Google 应用专用密码
// 的形状（界面把它分成四组显示，抄下来就带空格）。别的形状原样保留，
// 免得把真的含空格的自建邮箱密码改坏。
function compactAppPassword(value) {
  const trimmed = String(value || '').trim()
  const compacted = trimmed.replace(/\s+/g, '')
  return /^[A-Za-z0-9]{16}$/.test(compacted) ? compacted : trimmed
}

const createPreviewCount = computed(() => parseAddresses(createForm.raw).length)
const addressPreviewCount = computed(() => parseAddresses(addressForm.raw).length)
const activeHost = computed(() => createForm.host.trim() || moduleInfo.value.host || '')
const batchPreview = computed(() => parseCredentialLines(batchForm.raw))

async function load(options = {}) {
  const silent = Boolean(options.silent)
  if (!silent) loading.value = true
  try {
    const params = new URLSearchParams({ preset: preset.value, page: String(page.value) })
    if (query.value.trim()) params.set('q', query.value.trim())
    const payload = await api(`/api/imap-accounts?${params.toString()}`)
    data.value = payload
    total.value = payload.total || 0
    totalPages.value = payload.total_pages || 1
    page.value = payload.page || 1
    // 翻页或搜索之后选中项可能已经不在当前页上。不清掉的话右侧会显示一个列表里
    // 根本看不到的母号，看起来像它被删了。
    if (selected.value) {
      const current = accounts.value.find((item) => item.id === selected.value.id)
      if (current) selected.value = current
      else selected.value = null
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

function selectAccount(account) {
  selected.value = account
}

// 点已经展开的那一行要收起。默认不选中任何行，也就是默认折叠——与其余服务商页面一致。
function toggleAccount(account) {
  selected.value = selected.value?.id === account.id ? null : account
}

function openCreateDialog() {
  createForm.email = ''
  createForm.secret = ''
  createForm.host = ''
  createForm.port = 0
  createForm.username = ''
  createForm.label = ''
  createForm.note = ''
  createForm.raw = ''
  showCreate.value = true
}

function openAddAddressDialog() {
  addressForm.raw = ''
  showAddAddress.value = true
}

function openBatchDialog() {
  batchForm.raw = ''
  batchForm.host = ''
  batchForm.port = 0
  batchResult.value = null
  showBatch.value = true
}

// 选文件只是把文件内容读进文本框，不做别的：读进来的东西和粘贴进去的完全一样，
// 使用者可以在提交前看见并改，而不是点一下就直接导入一批看不见的内容。
async function onBatchFile(event) {
  const file = event.target.files?.[0]
  event.target.value = ''
  if (!file) return
  try {
    batchForm.raw = await file.text()
    batchResult.value = null
  } catch (err) {
    flash(`读不了这个文件：${err.message}`, true)
  }
}

async function submitBatch() {
  if (!batchPreview.value.items.length) {
    flash('没能从这些内容里解析出任何「邮箱:授权码」', true)
    return
  }
  if (!startBusy('batch')) return
  try {
    const payload = await api('/api/imap-accounts/batch', {
      method: 'POST',
      body: JSON.stringify({
        preset: preset.value,
        host: batchForm.host.trim(),
        port: Number(batchForm.port) || 0,
        raw: batchForm.raw,
      }),
    })
    const result = payload.result || {}
    batchResult.value = result
    const parts = [`成功 ${result.accounts || 0} 个`]
    if (result.failed) parts.push(`失败 ${result.failed} 个`)
    if (payload.skipped_lines) parts.push(`跳过无法解析的 ${payload.skipped_lines} 行`)
    flash(`${presetLabel.value}批量添加：${parts.join('，')}`, Boolean(result.failed))
    await load()
    // 全成功就直接关掉；有失败就留着，让使用者看到是哪几行、为什么。
    if (!result.failed) {
      showBatch.value = false
      batchResult.value = null
    }
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('batch')
  }
}

async function createAccount() {
  if (!createForm.email.trim()) {
    flash('请填写邮箱地址', true)
    return
  }
  if (!createForm.secret.trim()) {
    flash('请填写客户端授权码（不是邮箱登录密码）', true)
    return
  }
  if (!startBusy('create')) return
  try {
    const payload = await api('/api/imap-accounts', {
      method: 'POST',
      body: JSON.stringify({
        preset: preset.value,
        email: createForm.email.trim(),
        secret: createForm.secret.trim(),
        host: createForm.host.trim(),
        port: Number(createForm.port) || 0,
        username: createForm.username.trim(),
        label: createForm.label.trim(),
        note: createForm.note.trim(),
        raw: createForm.raw,
      }),
    })
    const result = payload.result || {}
    const parts = [`新增 ${result.created || 0} 个地址`, `更新 ${result.updated || 0} 个`]
    if (result.failed) parts.push(`失败 ${result.failed} 个`)
    if (payload.skipped_lines) parts.push(`跳过无法解析的 ${payload.skipped_lines} 条`)
    flash(`${presetLabel.value}已添加：${parts.join('，')}`, Boolean(result.failed))
    showCreate.value = false
    await load()
    if (payload.account) selected.value = accounts.value.find((item) => item.id === payload.account.id) || payload.account
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('create')
  }
}

async function addAddresses() {
  if (!selected.value) return
  if (!addressPreviewCount.value) {
    flash('没能从这些内容里解析出任何邮箱地址', true)
    return
  }
  if (!startBusy('add-address')) return
  try {
    const payload = await api(`/api/imap-accounts/${selected.value.id}/addresses`, {
      method: 'POST',
      body: JSON.stringify({ raw: addressForm.raw }),
    })
    const result = payload.result || {}
    const parts = [`新增 ${result.created || 0} 个`, `更新 ${result.updated || 0} 个`]
    if (result.failed) parts.push(`失败 ${result.failed} 个`)
    flash(`地址已添加：${parts.join('，')}`, Boolean(result.failed))
    showAddAddress.value = false
    await load({ silent: true })
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('add-address')
  }
}

async function syncAccount(account) {
  const target = account || selected.value
  if (!target) return
  if (!startBusy(`sync:${target.id}`)) return
  try {
    const payload = await api(`/api/imap-accounts/${target.id}/sync`, { method: 'POST' })
    const stored = payload.stored || 0
    flash(stored > 0 ? `已取到 ${stored} 封新邮件` : '已同步，暂无新邮件')
    await load({ silent: true })
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy(`sync:${target.id}`)
  }
}

async function togglePool(mailbox) {
  if (!startBusy(`pool:${mailbox.id}`)) return
  try {
    const payload = await api(`/api/mailboxes/${mailbox.id}/pool`, {
      method: 'POST',
      body: JSON.stringify({ in_pool: Boolean(mailbox.pool_hidden) }),
    })
    flash(payload.in_pool ? `${mailbox.email} 已放进邮箱池` : `${mailbox.email} 已移出邮箱池，只在本页可见`)
    await load({ silent: true })
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy(`pool:${mailbox.id}`)
  }
}

async function deleteAccount(account) {
  const name = account.label || account.apple_id
  const ok = await confirmAction({
    title: `删除 ${presetLabel.value}`,
    message: `将删除 ${name} 及其名下 ${account.mailbox_count || 0} 个地址和已收邮件，且不可恢复。`,
    confirmText: '删除',
  })
  if (!ok) return
  if (!startBusy(`delete:${account.id}`)) return
  try {
    const payload = await api(`/api/imap-accounts/${account.id}`, { method: 'DELETE' })
    flash(`已删除 ${name}；清理地址 ${payload.mailboxes || 0} 个，邮件 ${payload.messages || 0} 封`)
    if (selected.value?.id === account.id) selected.value = null
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy(`delete:${account.id}`)
  }
}

// 勾选：批量同步和批量入池作用在勾中的母号上；一个都没勾时按当前筛选条件的全部算。
// 两种都要——挑几个处理，和「别再让我点 456 次」都是真实需求。
const checkedIDs = ref([])
const allPageChecked = computed(() => accounts.value.length > 0 && accounts.value.every((account) => checkedIDs.value.includes(account.id)))
const somePageChecked = computed(() => !allPageChecked.value && accounts.value.some((account) => checkedIDs.value.includes(account.id)))

// 批量操作的进度。后端一次只处理一小批，这里循环调用并报出已完成的数量：
// 几分钟不吭声的一次性请求既看不到进度，也容易被浏览器或代理掐断。
const batch = reactive({ running: false, label: '', done: 0, total: 0, failed: 0 })
// 每批多少个母号。太小则往返次数多，太大则单次请求又变成「几分钟不吭声」。
const BATCH_SYNC_CHUNK = 10

function togglePageSelection() {
  const ids = accounts.value.map((account) => account.id)
  if (allPageChecked.value) checkedIDs.value = checkedIDs.value.filter((id) => !ids.includes(id))
  else checkedIDs.value = [...new Set([...checkedIDs.value, ...ids])]
}

function toggleAccountSelection(account) {
  checkedIDs.value = checkedIDs.value.includes(account.id)
    ? checkedIDs.value.filter((id) => id !== account.id)
    : [...checkedIDs.value, account.id]
}

// 「当前筛选下的全部」由服务端一次查出来，再原样传回去执行：确认框里显示的那个数
// 和实际动的那个数是同一批标识，不会出现「说 456 个、实际动了 461 个」。
async function resolveTargetIDs() {
  if (checkedIDs.value.length) return [...checkedIDs.value]
  const params = new URLSearchParams({ preset: preset.value })
  if (query.value.trim()) params.set('q', query.value.trim())
  const payload = await api(`/api/imap-accounts/ids?${params.toString()}`)
  return payload.ids || []
}

// 按钮的悬停说明必须说清这一次会动到多少个：勾选和「当前筛选全部」的区别，
// 在界面上只体现为这一行字。使用者点下去之前就该知道自己将影响几个母号。
function batchScopeTitle(action) {
  if (checkedIDs.value.length) return `对已勾选的 ${checkedIDs.value.length} 个母号${action}`
  return `对当前筛选下的全部 ${total.value} 个母号${action}`
}

async function runBatchSync() {
  if (batch.running) return
  let ids
  try {
    ids = await resolveTargetIDs()
  } catch (err) {
    flash(err.message, true)
    return
  }
  if (!ids.length) {
    flash('当前筛选下没有母号', true)
    return
  }
  const ok = await confirmAction({
    title: `批量同步${presetLabel.value}`,
    message: `将逐个读取 ${ids.length} 个母号名下所有地址的收件箱。数量多时需要几分钟，期间请不要关闭页面。`,
    confirmText: '开始同步',
  })
  if (!ok) return
  batch.running = true
  batch.label = '同步'
  batch.done = 0
  batch.failed = 0
  batch.total = ids.length
  let synced = 0
  try {
    for (let start = 0; start < ids.length; start += BATCH_SYNC_CHUNK) {
      const chunk = ids.slice(start, start + BATCH_SYNC_CHUNK)
      const payload = await api('/api/imap-accounts/sync-batch', {
        method: 'POST',
        body: JSON.stringify({ account_ids: chunk }),
      })
      synced += payload.synced_messages || 0
      batch.failed += payload.failed || 0
      batch.done = Math.min(start + chunk.length, ids.length)
    }
    flash(batch.failed
      ? `批量同步完成：成功 ${ids.length - batch.failed} 个，失败 ${batch.failed} 个，共取到 ${synced} 封`
      : `批量同步完成：${ids.length} 个母号全部成功，共取到 ${synced} 封`)
  } catch (err) {
    // 中断时如实说出停在哪里，已经同步好的那些是有效的，重跑不会重复收信。
    flash(`批量同步中断于 ${batch.done}/${ids.length}：${err.message}`, true)
  } finally {
    batch.running = false
    checkedIDs.value = []
    await load({ silent: true })
  }
}

async function runBatchPool(inPool) {
  if (batch.running) return
  let ids
  try {
    ids = await resolveTargetIDs()
  } catch (err) {
    flash(err.message, true)
    return
  }
  if (!ids.length) {
    flash('当前筛选下没有母号', true)
    return
  }
  const ok = await confirmAction({
    title: inPool ? '批量放进邮箱池' : '批量移出邮箱池',
    message: inPool
      ? `将把 ${ids.length} 个母号名下的全部地址加入邮箱池。`
      : `将把 ${ids.length} 个母号名下的全部地址移出邮箱池，之后它们只在母号页面可见。`,
    confirmText: inPool ? '加入邮箱池' : '移出邮箱池',
  })
  if (!ok) return
  batch.running = true
  batch.label = inPool ? '入池' : '出池'
  batch.done = 0
  batch.failed = 0
  batch.total = ids.length
  try {
    const payload = await api('/api/imap-accounts/pool-batch', {
      method: 'POST',
      body: JSON.stringify({ account_ids: ids, in_pool: inPool }),
    })
    batch.done = ids.length
    flash(`${ids.length} 个母号已处理，${payload.updated_mailboxes || 0} 个地址${inPool ? '加入' : '移出'}邮箱池`)
  } catch (err) {
    flash(err.message, true)
  } finally {
    batch.running = false
    checkedIDs.value = []
    await load({ silent: true })
  }
}

function scheduleRealtimeRefresh(change) {
  if (change.resource !== 'mailbox' && change.resource !== 'apple-account') return
  window.clearTimeout(realtimeRefreshTimer)
  realtimeRefreshTimer = window.setTimeout(() => load({ silent: true }), 120)
}

onMounted(async () => {
  await load()
  realtimeUnsubscribe = subscribeRealtime(scheduleRealtimeRefresh)
})

// QQ / 163 / Gmail 三条路由共用这一个组件，Vue 会复用同一个实例：切过去时
// onMounted 不会再跑一次。不在这里重新加载，页面就会「标题跟着路由变了、
// 列表和说明还是上一个服务商的」，看起来像串页。
//
// 先清空再拉，而不是等新数据回来再替换：否则加载期间页面上还挂着上一个
// 服务商的列表和限额说明，正是要修的那个现象。
watch(preset, () => {
  selected.value = null
  page.value = 1
  query.value = ''
  // 勾选跟着 preset 清空：QQ 页面上勾的母号在 163 页面上不该还生效。
  // 换页和搜索则刻意保留勾选——跨页挑选本来就是勾选存在的意义。
  checkedIDs.value = []
  data.value = { items: [], module: {} }
  load()
})

// 搜索防抖：每敲一个字就发一次请求既浪费，也会让列表在打字时不停闪。
// 搜索时回到第一页——停在第五页搜一个只有两条结果的词，会看到空列表。
watch(query, () => {
  window.clearTimeout(searchTimer)
  searchTimer = window.setTimeout(() => {
    page.value = 1
    load()
  }, 300)
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
        <div class="apple-command-title"><span><MailOpen :size="16" /></span><div><h2>{{ presetName }}</h2><p>用标准 IMAP + 客户端授权码收信，名下地址默认只在本页</p></div></div>
        <div class="apple-command-actions">
          <button class="primary-button apple-command-button" title="添加一个邮箱，并把它的地址一起导入" @click="openCreateDialog"><Plus :size="14" />添加{{ presetLabel }}</button>
          <button class="secondary-button apple-command-button" title="一次粘贴多个邮箱，每行「邮箱:授权码」，每行都是独立的母号" @click="openBatchDialog"><FileUp :size="14" />批量添加</button>
          <button class="secondary-button apple-command-button" :disabled="!selected || isBusy(`sync:${selected.id}`)" :title="selected ? '读一次该邮箱名下所有地址的收件箱' : '请先选择一个邮箱'" @click="syncAccount()"><LoaderCircle v-if="selected && isBusy(`sync:${selected.id}`)" :size="14" class="animate-spin" /><RefreshCw v-else :size="14" />同步此邮箱</button>
          <button class="secondary-button apple-command-button" :disabled="batch.running || (!checkedIDs.length && !total)" :title="batchScopeTitle('读一遍名下所有地址的收件箱')" @click="runBatchSync"><LoaderCircle v-if="batch.running && batch.label === '同步'" :size="14" class="animate-spin" /><RefreshCw v-else :size="14" />{{ batch.running && batch.label === '同步' ? `同步中 ${batch.done}/${batch.total}` : '批量同步' }}</button>
          <button class="secondary-button apple-command-button" :disabled="batch.running || (!checkedIDs.length && !total)" :title="batchScopeTitle('把名下所有地址加入邮箱池')" @click="runBatchPool(true)"><Inbox :size="14" />批量入池</button>
          <button class="secondary-button apple-command-button" :disabled="batch.running || (!checkedIDs.length && !total)" :title="batchScopeTitle('把名下所有地址移出邮箱池')" @click="runBatchPool(false)"><Inbox :size="14" />批量出池</button>
        </div>
      </header>

      <div v-if="batch.running || checkedIDs.length" class="apple-module-note">
        <LoaderCircle v-if="batch.running" :size="14" class="animate-spin" />
        <AlertCircle v-else :size="14" />
        <span v-if="batch.running">正在批量{{ batch.label }}：{{ batch.done }} / {{ batch.total }}<template v-if="batch.failed">，已失败 {{ batch.failed }}</template></span>
        <span v-else>已勾选 {{ checkedIDs.length }} 个母号，批量操作只作用在它们身上；一个都不勾则按当前筛选下的全部 {{ total }} 个算。</span>
        <button v-if="!batch.running" type="button" class="secondary-button apple-check-button" @click="checkedIDs = []">清除勾选</button>
      </div>

      <div v-if="moduleInfo.limit" class="apple-module-note">
        <AlertCircle :size="14" />
        <span>{{ moduleInfo.limit }}</span>
      </div>

      <div v-if="loading" class="apple-loading"><LoaderCircle :size="16" class="animate-spin" />正在加载{{ presetLabel }}</div>
      <div v-else class="apple-account-body">
        <section class="apple-account-list">
          <header class="apple-section-heading"><div><h3>{{ presetLabel }}</h3><p>选中一个后在它下方展开授权、名下地址与取信信息</p></div><label class="field-wrap mailbox-search"><Search :size="15" class="field-icon" /><input v-model="query" class="field field-leading" type="search" placeholder="搜索邮箱或标签" aria-label="搜索邮箱或标签" /></label><span>总 {{ total }} 个</span></header>
          <div v-if="!accounts.length" class="apple-empty"><span><Server :size="20" /></span><strong>{{ query.trim() ? '没有匹配的' + presetLabel : '还没有 ' + presetLabel }}</strong><small>{{ query.trim() ? '换个关键词试试，或清空搜索框。' : `点击“添加${presetLabel}”，填入邮箱地址和客户端授权码。` }}</small></div>
          <div v-else class="apple-account-table-viewport">
            <table class="apple-account-table">
              <colgroup><col class="apple-col-check" /><col class="apple-col-account" /><col class="apple-col-status" /><col class="apple-col-channel" /><col class="apple-col-action" /></colgroup>
              <thead><tr><th class="apple-check-cell"><input type="checkbox" :checked="allPageChecked" :indeterminate="somePageChecked" :disabled="!accounts.length" title="全选本页" aria-label="全选本页母号" @change="togglePageSelection" /></th><th>邮箱</th><th>状态</th><th>地址</th><th>操作</th></tr></thead>
              <tbody>
                <template v-for="account in accounts" :key="account.id">
                <tr class="apple-account-row" :class="{ 'apple-account-row-selected': selected?.id === account.id }" tabindex="0" :aria-expanded="selected?.id === account.id" :aria-label="`查看 ${account.label || account.apple_id} 的地址`" @click="toggleAccount(account)" @keydown.enter="toggleAccount(account)" @keydown.space.prevent="toggleAccount(account)">
                  <td class="apple-check-cell"><input type="checkbox" :checked="checkedIDs.includes(account.id)" :aria-label="`勾选 ${account.label || account.apple_id}`" @click.stop @change="toggleAccountSelection(account)" /></td>
                  <td><button type="button" class="apple-account-select" @click.stop="selectAccount(account)"><span class="apple-account-icon"><MailOpen :size="14" /></span><span><strong>{{ account.label || account.apple_id }}</strong><small>{{ account.apple_id }}</small></span></button></td>
                  <td><span :class="statusClass(account)" class="apple-status-badge">{{ statusLabel(account.icloud_status || account.status) }}</span></td>
                  <td><div class="apple-channel-list"><span class="apple-channel-pill"><Inbox :size="11" /><span>{{ account.mailbox_count || 0 }} 个地址</span><em v-if="account.pool_count" class="bg-sky-50 text-sky-600 dark:bg-sky-950/30 dark:text-sky-300">其中 {{ account.pool_count }} 个进池</em></span></div></td>
                  <td><button type="button" class="apple-delete-button" :title="`删除 ${account.label || account.apple_id}`" :aria-label="`删除 ${account.label || account.apple_id}`" :disabled="isBusy(`delete:${account.id}`)" @click.stop="deleteAccount(account)"><LoaderCircle v-if="isBusy(`delete:${account.id}`)" :size="13" class="animate-spin" /><Trash2 v-else :size="13" /></button></td>
                </tr>
                <tr v-if="selected?.id === account.id" class="apple-account-expand-row">
                  <td colspan="5" class="apple-account-expand-cell">
                    <div class="apple-account-expand-body">
                      <!-- ① 授权与服务器：折叠 + 就地可同步。 -->
                      <section class="detail-section">
                        <header class="detail-section-head">
                          <button type="button" class="detail-section-toggle" :aria-expanded="openSections.secret" @click="openSections.secret = !openSections.secret">
                            <ChevronRight :size="15" class="detail-section-caret" />
                            <span class="detail-section-title">
                              <strong><KeyRound :size="15" />授权与服务器</strong>
                              <small>{{ selected.label || selected.apple_id }} · 授权码与 IMAP 地址</small>
                            </span>
                          </button>
                          <div class="detail-section-actions">
                            <button class="secondary-button apple-check-button" :disabled="isBusy(`sync:${selected.id}`)" @click="syncAccount()"><LoaderCircle v-if="isBusy(`sync:${selected.id}`)" :size="13" class="animate-spin" /><RefreshCw v-else :size="13" />{{ isBusy(`sync:${selected.id}`) ? '同步中' : '立即同步' }}</button>
                          </div>
                        </header>
                        <div v-if="openSections.secret" class="detail-section-body">
                          <div class="apple-state-list">
                            <article class="apple-state-row">
                              <span class="apple-state-icon text-sky-600 bg-sky-50 dark:bg-sky-950/30 dark:text-sky-300"><KeyRound :size="14" /></span>
                              <span class="apple-state-copy"><strong>客户端授权码</strong><small>等同于邮箱密码，落库加密存储</small></span>
                              <span :class="statusClass(selected)" class="apple-state-status">{{ statusLabel(selected.icloud_status || selected.status) }}</span>
                            </article>
                            <article class="apple-state-row">
                              <span class="apple-state-icon text-violet-600 bg-violet-50 dark:bg-violet-950/30 dark:text-violet-300"><Server :size="14" /></span>
                              <span class="apple-state-copy"><strong>{{ selected.imap_host }}:{{ selected.imap_port || 993 }}</strong><small>账号名 {{ selected.imap_username || selected.apple_id }}</small></span>
                              <span class="apple-state-status bg-emerald-50 text-emerald-600 dark:bg-emerald-950/30 dark:text-emerald-300">IMAP</span>
                            </article>
                          </div>
                        </div>
                      </section>

                      <!-- ② 邮件与验证码：默认折叠，内容由 AccountInboxPanel 自己渲染。 -->
                      <AccountInboxPanel collapsible :account-id="selected?.id || ''" :account-name="selected?.label || selected?.apple_id || ''" />

                      <!-- ③ 名下地址：列全名下的地址，逐个决定要不要进邮箱池。 -->
                      <section class="detail-section">
                        <header class="detail-section-head">
                          <button type="button" class="detail-section-toggle" :aria-expanded="openSections.addresses" @click="openSections.addresses = !openSections.addresses">
                            <ChevronRight :size="15" class="detail-section-caret" />
                            <span class="detail-section-title">
                              <strong><MailOpen :size="15" />名下地址</strong>
                              <small>{{ selected.mailbox_count || 0 }} 个地址 · 其中 {{ selected.pool_count || 0 }} 个在邮箱池 · 默认不进池，需要时逐个勾选</small>
                            </span>
                          </button>
                          <div class="detail-section-actions">
                            <button class="secondary-button apple-check-button" @click="openAddAddressDialog"><Plus :size="13" />添加地址</button>
                          </div>
                        </header>
                        <div v-if="openSections.addresses" class="detail-section-body">
                          <div v-if="!selected.mailboxes?.length" class="apple-state-empty">这个邮箱下还没有地址。</div>
                          <ul v-else class="apple-alias-list">
                            <li v-for="mailbox in selected.mailboxes" :key="mailbox.id">
                              <MailOpen :size="13" />
                              <span class="font-mono">{{ mailbox.email }}</span>
                              <button type="button" class="secondary-button apple-check-button" :disabled="isBusy(`pool:${mailbox.id}`)" :title="mailbox.pool_hidden ? '放进邮箱池' : '移出邮箱池，只在本页可见'" @click="togglePool(mailbox)">
                                <LoaderCircle v-if="isBusy(`pool:${mailbox.id}`)" :size="12" class="animate-spin" />
                                <CheckCircle2 v-else-if="!mailbox.pool_hidden" :size="12" />
                                <Plus v-else :size="12" />
                                {{ mailbox.pool_hidden ? '进池' : '已在池中' }}
                              </button>
                            </li>
                          </ul>
                        </div>
                      </section>

                      <!-- ④ 服务商说明：限制来自服务商本身，不是本程序的选择。 -->
                      <section class="detail-section">
                        <header class="detail-section-head">
                          <button type="button" class="detail-section-toggle" :aria-expanded="openSections.provider" @click="openSections.provider = !openSections.provider">
                            <ChevronRight :size="15" class="detail-section-caret" />
                            <span class="detail-section-title">
                              <strong><Server :size="15" />服务商说明</strong>
                              <small>这些限制来自服务商本身，不是本程序的选择</small>
                            </span>
                          </button>
                        </header>
                        <div v-if="openSections.provider" class="detail-section-body">
                          <dl class="apple-meta-list">
                            <div><dt>IMAP 服务器</dt><dd class="font-mono">{{ selected.imap_host }}:{{ selected.imap_port || 993 }}</dd></div>
                            <div><dt>地址数量</dt><dd>{{ selected.mailbox_count || 0 }} 个，其中 {{ selected.pool_count || 0 }} 个在邮箱池</dd></div>
                            <div><dt>已收邮件</dt><dd>{{ selected.message_count || 0 }} 封</dd></div>
                          </dl>
                          <p v-if="moduleInfo.hint" class="apple-detail-hint"><AlertCircle :size="13" />{{ moduleInfo.hint }}</p>
                        </div>
                      </section>
                    </div>
                  </td>
                </tr>
                </template>
              </tbody>
            </table>
          </div>
          <ListPager :page="page" :total-pages="totalPages" :total="total" unit="个母号" :loading="loading" @change="changePage" />
        </section>
      </div>
    </section>

    <div v-if="showCreate" class="mailbox-dialog-backdrop" role="presentation" @click.self="showCreate = false">
      <form class="panel mailbox-operation-dialog mailbox-import-dialog" role="dialog" aria-modal="true" aria-labelledby="imap-create-title" @submit.prevent="createAccount">
        <header class="mailbox-dialog-heading"><div><h2 id="imap-create-title"><Plus :size="18" />添加{{ presetLabel }}</h2><p>这里填的就是收信的那个邮箱，它的地址会被自动一起导入。</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('create')" @click="showCreate = false"><X :size="16" /></button></header>
        <div class="mailbox-dialog-grid">
          <label class="form-group"><span class="form-label">邮箱地址</span><input v-model.trim="createForm.email" class="field font-mono" :placeholder="preset === '163' ? 'someone@163.com' : '12345@qq.com'" /></label>
          <label class="form-group"><span class="form-label">客户端授权码</span><input v-model.trim="createForm.secret" class="field font-mono" type="password" placeholder="不是邮箱登录密码" /></label>
          <label class="form-group"><span class="form-label">IMAP 服务器</span><input v-model.trim="createForm.host" class="field" :placeholder="moduleInfo.host || 'imap.example.com'" /></label>
          <label class="form-group"><span class="form-label">端口</span><input v-model.number="createForm.port" class="field" type="number" :placeholder="String(moduleInfo.port || 993)" /></label>
          <label class="form-group"><span class="form-label">账号名</span><input v-model.trim="createForm.username" class="field" placeholder="留空则用邮箱地址本身" /></label>
          <label class="form-group"><span class="form-label">标签</span><input v-model.trim="createForm.label" class="field" placeholder="留空则用邮箱地址" /></label>
          <label class="form-group mailbox-dialog-wide"><span class="form-label">备注</span><input v-model.trim="createForm.note" class="field" placeholder="可选" /></label>
          <div class="form-group mailbox-dialog-wide">
            <span class="form-label">名下其它地址（可选，一行一个）</span>
            <textarea v-model="createForm.raw" class="field font-mono" rows="4" placeholder="别名地址，留空就只导入这一个地址"></textarea>
          </div>
        </div>
        <p v-if="moduleInfo.hint" class="apple-dialog-intro"><strong>怎么拿授权码：</strong>{{ moduleInfo.hint }}</p>
        <p v-if="moduleInfo.limit" class="apple-dialog-intro"><strong>能挂多少地址：</strong>{{ moduleInfo.limit }}</p>
        <p class="apple-dialog-intro">已解析出 <strong>{{ createPreviewCount }}</strong> 条附加地址（邮箱自己那一条由服务端补上，不占这个数）。授权码等同于邮箱密码，保存时会加密。导入时会先探活一次并如实报告结果。</p>
        <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('create')" @click="showCreate = false">取消</button><button class="primary-button" :disabled="isBusy('create') || !createForm.email.trim() || !createForm.secret.trim()"><LoaderCircle v-if="isBusy('create')" :size="15" class="animate-spin" /><Plus v-else :size="15" />添加{{ presetLabel }}</button></footer>
      </form>
    </div>

    <div v-if="showBatch" class="mailbox-dialog-backdrop" role="presentation" @click.self="showBatch = false">
      <form class="panel mailbox-operation-dialog mailbox-import-dialog" role="dialog" aria-modal="true" aria-labelledby="imap-batch-title" @submit.prevent="submitBatch">
        <header class="mailbox-dialog-heading"><div><h2 id="imap-batch-title"><FileUp :size="18" />批量添加{{ presetLabel }}</h2><p>一行一个邮箱，每行都是它自己的母号，各有各的授权码。一行失败不影响其它行。</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('batch')" @click="showBatch = false"><X :size="16" /></button></header>
        <div class="mailbox-dialog-grid">
          <div class="form-group mailbox-dialog-wide">
            <span class="form-label">邮箱:授权码（一行一个）</span>
            <textarea v-model="batchForm.raw" class="field font-mono" rows="8" placeholder="someone@gmail.com:abcdefghijklmnop&#10;another@gmail.com:qrstuvwxyzabcdef"></textarea>
          </div>
          <label class="form-group"><span class="form-label">IMAP 服务器</span><input v-model.trim="batchForm.host" class="field" :placeholder="moduleInfo.host || 'imap.example.com'" /></label>
          <label class="form-group"><span class="form-label">端口</span><input v-model.number="batchForm.port" class="field" type="number" :placeholder="String(moduleInfo.port || 993)" /></label>
        </div>
        <div class="flex flex-wrap items-center gap-3">
          <button type="button" class="secondary-button" :disabled="isBusy('batch')" @click="batchFileInput?.click()"><FileUp :size="14" />选择文件</button>
          <input ref="batchFileInput" type="file" accept=".txt,.csv,.tsv,text/plain" class="hidden" @change="onBatchFile" />
          <span class="text-[10px]">已解析出 <strong>{{ batchPreview.items.length }}</strong> 个账号<template v-if="batchPreview.skipped">，跳过 <strong>{{ batchPreview.skipped }}</strong> 行</template></span>
        </div>
        <p v-if="moduleInfo.hint" class="apple-dialog-intro"><strong>怎么拿授权码：</strong>{{ moduleInfo.hint }}</p>
        <p class="apple-dialog-intro">每一行都会单独探活一次，所以行数多时要等一会儿；失败的那几行会留在下面，写清是哪一行、为什么。授权码等同于邮箱密码，保存时会加密。</p>
        <ul v-if="batchResult?.items?.length" class="m-0 grid list-none gap-1.5 p-0">
          <li v-for="item in batchResult.items" :key="item.email" class="flex items-start gap-2 rounded-lg border border-slate-200 bg-slate-50 px-2.5 py-1.5 text-[10px] dark:border-slate-700 dark:bg-slate-900/40">
            <CheckCircle2 v-if="item.ok" :size="13" class="mt-0.5 shrink-0 text-emerald-600 dark:text-emerald-400" />
            <AlertCircle v-else :size="13" class="mt-0.5 shrink-0 text-rose-600 dark:text-rose-400" />
            <span class="font-mono">{{ item.email }}</span>
            <span class="text-slate-500 dark:text-slate-400">{{ item.ok ? '已添加' : item.reason }}</span>
          </li>
        </ul>
        <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('batch')" @click="showBatch = false">取消</button><button class="primary-button" :disabled="isBusy('batch') || !batchPreview.items.length"><LoaderCircle v-if="isBusy('batch')" :size="15" class="animate-spin" /><FileUp v-else :size="15" />添加 {{ batchPreview.items.length }} 个账号</button></footer>
      </form>
    </div>

    <div v-if="showAddAddress" class="mailbox-dialog-backdrop" role="presentation" @click.self="showAddAddress = false">
      <form class="panel mailbox-operation-dialog mailbox-import-dialog" role="dialog" aria-modal="true" aria-labelledby="imap-address-title" @submit.prevent="addAddresses">
        <header class="mailbox-dialog-heading"><div><h2 id="imap-address-title"><Plus :size="18" />给 {{ selected?.label || selected?.apple_id }} 添加地址</h2><p>沿用这个邮箱已保存的授权码，不用再填一次。</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('add-address')" @click="showAddAddress = false"><X :size="16" /></button></header>
        <div class="mailbox-dialog-grid">
          <div class="form-group mailbox-dialog-wide">
            <span class="form-label">邮箱地址（一行一个）</span>
            <textarea v-model="addressForm.raw" class="field font-mono" rows="6" :placeholder="activeHost ? `别名地址，例如 ${preset === '163' ? 'someone@163.com' : '12345@qq.com'}` : '别名地址'"></textarea>
          </div>
        </div>
        <p class="apple-dialog-intro">已解析出 <strong>{{ addressPreviewCount }}</strong> 条。新地址默认不进邮箱池，只在本页可见；要放进池子，在上面的地址列表里点「进池」。</p>
        <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('add-address')" @click="showAddAddress = false">取消</button><button class="primary-button" :disabled="isBusy('add-address') || !addressPreviewCount"><LoaderCircle v-if="isBusy('add-address')" :size="15" class="animate-spin" /><Plus v-else :size="15" />添加 {{ addressPreviewCount }} 个地址</button></footer>
      </form>
    </div>
  </div>
</template>
