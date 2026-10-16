<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { Boxes, ChevronLeft, ChevronRight, ChevronsLeft, ChevronsRight, Clipboard, CloudDownload, CloudOff, FileUp, KeyRound, LoaderCircle, MailOpen, MailPlus, RefreshCw, Save, Search, ShieldCheck, ShieldX, Trash2, X } from '@lucide/vue'
import { api } from '../api/client'
import CardSelect from '../components/CardSelect.vue'
import FormDialog from '../components/FormDialog.vue'
import { useConfirm } from '../composables/useConfirm'
import { subscribeRealtime } from '../composables/useRealtime'
import { useToast } from '../composables/useToast'

const loading = ref(true)
const loadingVisible = ref(false)
const busyActions = ref([])
const codeBusyVisible = ref('')
const query = ref('')
const accountID = ref('')
const status = ref('')
const page = ref(1)
const pageSize = ref(7)
const result = ref({ items: [], page: 1, page_size: 7, total: 0, total_pages: 1 })
const selected = ref(null)
const selectedMessage = ref(null)
const selectedMessageLoading = ref(false)
const messageViewMode = ref('auto')
const messages = ref([])
const code = ref(null)
const codeMailbox = ref(null)
const codeError = ref('')
const codeDialogOpen = ref(false)
const rowBusyActions = ref({})
const selectedMailboxes = ref([])
const accounts = ref([])
const showImport = ref(false)
// 外部取码邮箱：没有母号、没有登录密码，只有一个取码地址。
const showExternalImport = ref(false)
const externalImport = reactive({ raw: '', label: '', note: '' })
const showSync = ref(false)
const showBulkDelete = ref(false)
const bulkDeleteEmails = ref('')
const bulkDeleteError = ref('')
const quickEditOpen = ref(false)
const quickEditField = ref('note')
const quickEditMailbox = ref(null)
const deleteConfirmID = ref('')
const deleteQueue = ref([])
const deletingMailboxIDs = ref([])
const edit = reactive({ status: 'available', api_active: true, icloud_active: true, note: '' })
const quickEdit = reactive({ status: 'available', note: '' })
const remoteClean = reactive({ move_synced: true, empty_trash: true })
const appleMailCleanup = ref({ running: false, status: 'idle', total_accounts: 0, total_mailboxes: 0, completed_mailboxes: 0, successful_mailboxes: 0, failed_mailboxes: 0, queued: 0, active: 0, completed: 0, success: 0, failed: 0 })
const existingMailboxMessageSync = ref({ running: false, status: 'idle', total_accounts: 0, total_mailboxes: 0, queued: 0, active: 0, completed_accounts: 0, successful_accounts: 0, failed_accounts: 0, imap_accounts: 0, web_api_accounts: 0, fallbacks: 0, scanned: 0, matched: 0, synced_messages: 0 })
const mailboxImport = reactive({ account_id: '', email: '', label: '', note: '' })
// 邮箱体检：逐批扫描全池，并把结果落到邮箱自己的状态上——否则「能不能用」得靠读报告才知道。
const healthCheck = ref({ running: false, done: 0, total: 0, report: null })
// 报告用弹窗显示。以前它内嵌在工具条下面，几十条读不了的地址会把页面撑长，
// 于是页面上多出一条滚动条，和邮箱列表自己那条抢滚轮。
const healthCheckOpen = ref(false)
const syncAccountID = ref('')
const mailboxCommandBar = ref(null)
const mailboxTableViewport = ref(null)
const mailboxPagination = ref(null)
const mailboxTableHeight = ref(372)
const mailboxEmptyHeight = ref(336)
let searchTimer
let loadingTimer
let codeBusyTimer
let autoRefreshTimer
let realtimeRefreshTimer
let realtimeUnsubscribe = () => {}
const pendingRealtimeResources = new Set()
let tableResizeTimer
let mailboxLayoutObserver
let loadRequestID = 0
let messageLoadRequestID = 0
let deleteQueueRunning = false
const maxConcurrentDeleteAccounts = 4
const activeDeleteAccounts = new Set()
let autoRefreshing = false
let deleteNoticeID = null
let deleteSucceeded = 0
let deleteFailed = 0
let deleteLastError = ''
let syncExistingNoticeID = null
let syncExistingMessagesNoticeID = null
let syncExistingMessagesIntroID = null
let syncExistingMessagesIntroTimer = null
let syncExistingMessagesStatusTimer = null
let syncExistingMessagesIntroVisible = false
let cleanAllNoticeID = null
let mailboxSyncBatch = null
const mailboxMessageSyncHint = '同步该邮箱所属 Apple 主号的所有新邮件；IMAP 主路径，iCloud Web 补查并自动合并'

// 上一次批量同步被打断时，做完的账号已经记进任务状态并落库，再点一次会从这里接着做，
// 不会从零重扫。所以这一次要做的会比账号总数少——按钮上必须说明，否则那看起来像漏算了。
const mailboxMessageSyncResumable = computed(() => {
  const job = existingMailboxMessageSync.value
  if (!job || job.running) return 0
  if (job.status !== 'interrupted' && job.status !== 'partial') return 0
  return Array.isArray(job.completed_account_ids) ? job.completed_account_ids.length : 0
})

const mailboxMessageSyncBulkHint = computed(() => {
  const base = '读取所有 Apple 主号在保留期以内的邮件；IMAP 主路径，iCloud Web 补查并自动合并。更早的邮件取回来也会被自动清理删掉'
  const resumed = mailboxMessageSyncResumable.value
  if (!resumed) return base
  return `${base}。上次同步中断过，已有 ${resumed} 个账号做完，再点会从这些账号之后接着做，不会重头再来`
})
const mailboxBackfillHint = '把该邮箱服务器上已经存在的旧邮件取回来：不看游标，也不看最近 24 小时。只取保留期以内的邮件——更早的取回来也会被自动维护删掉。邮件多时耗时较长，但只作用于这一个邮箱。'
const { success, error: showError, update: updateToast, dismiss: dismissToast } = useToast()
const { confirm: confirmAction } = useConfirm()
const mailboxStatusOptions = [
  { value: '', label: '全部状态', dot: 'bg-slate-400' },
  { value: 'available', label: '可用', dot: 'bg-emerald-500' },
  { value: 'reserved', label: '已预留', dot: 'bg-violet-500' },
  { value: 'used', label: '已使用', dot: 'bg-amber-500' },
  { value: 'failed', label: '失败', dot: 'bg-rose-500' },
  { value: 'disabled', label: '已停用', dot: 'bg-slate-500' },
]
const mailboxDetailStatusOptions = [
  { value: 'available', label: '可用', dot: 'bg-emerald-500' },
  { value: 'reserved', label: '已预留（由租约管理）', dot: 'bg-violet-500', disabled: true },
  { value: 'used', label: '已使用', dot: 'bg-amber-500' },
  { value: 'active', label: '活跃', dot: 'bg-sky-500' },
  { value: 'failed', label: '失败', dot: 'bg-rose-500' },
  { value: 'disabled', label: '已停用', dot: 'bg-slate-500' },
]
// 四个来源的账号来自各自的接口，这里合并成一个列表，每条打上 provider 标记。
// 筛选、分组、列显示和按账号的操作都依赖这个标记。
// IMAP（QQ/163/Gmail）也在其中：这一类占库里绝大多数邮箱，缺了它，
// 邮箱池里所有 imap 行都会显示「imap（账号未加载）」，账号筛选也筛不到它们。
const PROVIDER_LABELS = { icloud: 'iCloud', microsoft: 'Microsoft', google: 'Google', imap: 'QQ/163/Gmail' }
const ACCOUNT_ENDPOINTS = [
  ['icloud', '/api/apple-accounts'],
  ['microsoft', '/api/microsoft-accounts'],
  ['google', '/api/google-accounts'],
  ['imap', '/api/imap-accounts'],
]
function providerLabel(provider) {
  return PROVIDER_LABELS[provider] || provider || 'iCloud'
}
function accountName(account) {
  return account?.label || account?.apple_id || account?.id
}
function accountOptionLabel(account) {
  return `${accountName(account)}（${providerLabel(account?.provider)}）`
}
const accountOptions = computed(() => accounts.value.map((account) => ({ value: account.id, label: accountOptionLabel(account) })))
const accountByID = computed(() => new Map(accounts.value.map((account) => [account.id, account])))
const accountFilterOptions = computed(() => [
  { value: '', label: '全部账号', description: '显示所有服务商的邮箱', dot: 'bg-slate-400' },
  ...accounts.value.map((account) => ({
    value: account.id,
    label: accountOptionLabel(account),
    description: account.label && account.apple_id && account.label !== account.apple_id ? account.apple_id : `只显示此 ${providerLabel(account.provider)} 账号创建的邮箱`,
  })),
])
const syncAccountOptions = computed(() => [{ value: '', label: '全部账号', dot: 'bg-slate-400' }, ...accounts.value.map((account) => ({ value: account.id, label: `${accountName(account)}（${providerLabel(account.provider)}）` }))])
const hasRowBusyActions = computed(() => Object.keys(rowBusyActions.value).length > 0)
const codeDialogBusy = computed(() => isBusy('code') || Boolean(codeMailbox.value?.id && rowBusyAction(codeMailbox.value.id) === 'code'))
const quickEditTitle = computed(() => quickEditField.value === 'note' ? '修改备注' : '修改邮箱状态')
const bulkDeleteEmailCount = computed(() => parseBulkDeleteEmails(bulkDeleteEmails.value).length)
const selectedMailboxIDs = computed(() => selectedMailboxes.value.map((mailbox) => mailbox.id))
// 「删除选中」必须按每个邮箱自己所属的类别分流，用的规则和行内那个按钮完全一样：主地址不许删、
// Apple 的走远端彻底删除、其余的只删本地。从前这条路不看类别，把整批都当成 Apple 发出去，
// 于是选中 5 个 IMAP 记录得到的是「成功 0｜失败 5」和一串「对应 Apple 账号登录态不存在」。
const selectedDeletePlan = computed(() => {
  const plan = { remote: [], local: [], blocked: [], busy: [] }
  for (const mailbox of selectedMailboxes.value) {
    if (isMailboxDeleteBusy(mailbox.id)) plan.busy.push(mailbox)
    else plan[deleteModeFor(mailbox)].push(mailbox)
  }
  return plan
})
const selectedDeletableCount = computed(() => selectedDeletePlan.value.remote.length + selectedDeletePlan.value.local.length)
const selectedPageCount = computed(() => (result.value.items || []).filter((mailbox) => selectedMailboxIDs.value.includes(mailbox.id)).length)
const allPageMailboxesSelected = computed(() => Boolean(result.value.items?.length) && selectedPageCount.value === result.value.items.length)
const somePageMailboxesSelected = computed(() => selectedPageCount.value > 0 && !allPageMailboxesSelected.value)
const selectedMessageHasHTML = computed(() => Boolean(
  String(selectedMessage.value?.html_body || '').trim()
  || (String(selectedMessage.value?.content_type || '').toLowerCase().includes('text/html') && looksLikeHTML(selectedMessage.value?.body)),
))
const selectedMessageHTML = computed(() => {
  if (!selectedMessage.value) return ''
  return String(selectedMessage.value.html_body || (looksLikeHTML(selectedMessage.value.body) ? selectedMessage.value.body : '')).trim()
})
const selectedMessageHTMLDocument = computed(() => buildEmailHTMLDocument(selectedMessageHTML.value))
const showSelectedMessageHTML = computed(() => selectedMessageHasHTML.value && messageViewMode.value !== 'text')

function statusLabel(value) {
  return ({ available: '可用', reserved: '已预留', used: '已使用', failed: '失败', disabled: '已停用', active: '活跃' })[value] || value || '未知'
}

// 账号列：优先用账号列表里的名字；账号不在列表里时退回邮箱自己带的 provider 标记，
// 而不是把一串 UUID 直接显示出来。
function mailboxAppleAccount(mailbox) {
  const account = accountByID.value.get(mailbox?.account_id)
  if (account) return accountName(account)
  return mailbox?.account_id ? `${providerLabel(mailbox?.provider)}（账号未加载）` : '—'
}

function mailboxProvider(mailbox) {
  // 先看记录自己写的服务商：那是逐条的事实。账号表只是补充——apple_accounts 这张表里装着
  // 所有账号（458 条里有 IMAP 的），所以按 id 去账号表里查，可能查到一个属于别的服务商的条目。
  const own = String(mailbox?.provider || '').trim()
  if (own) return own
  const account = accountByID.value.get(mailbox?.account_id)
  const fromAccount = String(account?.provider || '').trim()
  if (fromAccount) return fromAccount
  // 认不出来就别猜。兜底成 icloud 会把一条普通邮箱送去走 Apple 的远端彻底删除：那一步需要
  // iCloud 登录态，它根本没有，于是只会失败；而如果哪天恰好有登录态，它会去删一个属于别处的
  // 隐私邮箱。认不出就按「没有远端可清理」处理，只删本地——这一步是可恢复的那一侧。
  return ''
}

// 彻底删除是 Apple 专有动作：它要 iCloud 登录态，还要在 Apple 服务器上删掉那个隐私邮箱。
// 对 Microsoft / Google / external 的记录调用它，只会在登录态那一步报错，什么都删不掉。
function supportsRemoteDelete(mailbox) {
  if (!mailbox) return false
  return mailboxProvider(mailbox) === 'icloud'
}

// 主地址那条池子记录删不得：Microsoft 走 Graph、Google 走 Gmail，两边都是按收件人字段
// 去池子里精确匹配的（protocol/graph_client.go 用池子记录建 byEmail 表）。少了这条记录，
// 寄到主地址的信会被算成「未匹配」直接丢掉——主信箱静默停收。
function isPrimaryMailbox(mailbox) {
  return String(mailbox?.remote_origin || '').toLowerCase().endsWith('_primary')
}

// 一个邮箱该走哪条删除路径。行内那个按钮和「删除选中」都从这里取，两边再也分不出规则差异——
// 这条规则原本只写在行内的分支里，批量那条路各写各的，于是漏掉了服务商。
function deleteModeFor(mailbox) {
  if (!mailbox || isPrimaryMailbox(mailbox)) return 'blocked'
  return supportsRemoteDelete(mailbox) ? 'remote' : 'local'
}

// 「清理与删除」区块的说明文字必须和那一格实际给出的按钮一致：主地址连「只删本地」
// 都没有，就不能拿「只删本地会先清空本地邮件」去描述它。
function deleteSectionNote(mailbox) {
  if (isPrimaryMailbox(mailbox)) {
    return '主地址就是这一行账号本身。删掉这条池子记录后，寄到主地址的信会因为匹配不到任何本地地址而被丢弃，所以这里不提供删除。'
  }
  if (supportsRemoteDelete(mailbox)) {
    return '彻底删除会精确清理本地已同步的远端邮件并清空所属账号废纸篓，再删除 Apple 隐私邮箱及本地记录。'
  }
  return '这类邮箱没有 Apple 远端可清理：「只删本地」会先清空本地邮件，再删掉本地邮箱记录。'
}

function statusClass(value) {
  if (value === 'available' || value === 'active') return 'bg-emerald-50 text-emerald-600 dark:bg-emerald-950/40 dark:text-emerald-300'
  if (value === 'reserved') return 'bg-violet-50 text-violet-600 dark:bg-violet-950/40 dark:text-violet-300'
  if (value === 'failed') return 'bg-rose-50 text-rose-600 dark:bg-rose-950/40 dark:text-rose-300'
  if (value === 'disabled') return 'bg-slate-100 text-slate-500 dark:bg-slate-700 dark:text-slate-300'
  return 'bg-amber-50 text-amber-600 dark:bg-amber-950/40 dark:text-amber-300'
}

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

async function copyText(value) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value)
    return
  }
  const textarea = document.createElement('textarea')
  textarea.value = value
  textarea.setAttribute('readonly', '')
  textarea.style.position = 'fixed'
  textarea.style.opacity = '0'
  document.body.appendChild(textarea)
  textarea.select()
  const copied = document.execCommand('copy')
  textarea.remove()
  if (!copied) throw new Error('复制失败')
}

async function copyMailboxEmail(mailbox) {
  try {
    await copyText(mailbox.email)
    flash(`邮箱已复制：${mailbox.email}`)
  } catch (err) {
    flash(err.message || '复制邮箱失败，请重试', true)
  }
}

function isMailboxSelected(mailboxID) {
  return selectedMailboxIDs.value.includes(mailboxID)
}

// 选择集必须存整行。从前这里只留 id 和 email，于是「删除选中」拿到手的对象没有服务商、
// 也没有所属账号，逐条判断只能兜底成 icloud，把一批普通邮箱当成 Apple 去走远端彻底删除——
// 每一项都在「对应 Apple 账号登录态不存在」那一步失败。行内那个按钮传的是整行，所以它一直是对的：
// 同一条规则，两条路径拿到的数据不一样，结果就不一样。
function toggleMailboxSelection(mailbox) {
  if (!mailbox || isMailboxDeleteBusy(mailbox.id)) return
  selectedMailboxes.value = isMailboxSelected(mailbox.id)
    ? selectedMailboxes.value.filter((item) => item.id !== mailbox.id)
    : [...selectedMailboxes.value, { ...mailbox }]
}

function toggleAllMailboxSelection() {
  const pageMailboxes = result.value.items || []
  if (!pageMailboxes.length) return
  const pageIDs = new Set(pageMailboxes.map((mailbox) => mailbox.id))
  if (allPageMailboxesSelected.value) {
    selectedMailboxes.value = selectedMailboxes.value.filter((mailbox) => !pageIDs.has(mailbox.id))
    return
  }
  const selectedIDs = new Set(selectedMailboxIDs.value)
  selectedMailboxes.value = [
    ...selectedMailboxes.value,
    ...pageMailboxes.filter((mailbox) => !selectedIDs.has(mailbox.id)).map((mailbox) => ({ ...mailbox })),
  ]
}

function unselectMailbox(mailboxID) {
  selectedMailboxes.value = selectedMailboxes.value.filter((mailbox) => mailbox.id !== mailboxID)
}

function startCodeBusy(key) {
  if (!startBusy(key)) return false
  codeBusyVisible.value = ''
  clearTimeout(codeBusyTimer)
  codeBusyTimer = window.setTimeout(() => {
    if (isBusy(key)) codeBusyVisible.value = key
  }, 600)
  return true
}

function finishCodeBusy(key) {
  clearTimeout(codeBusyTimer)
  finishBusy(key)
  if (codeBusyVisible.value === key) codeBusyVisible.value = ''
}

function rowBusyAction(mailboxID) {
  return rowBusyActions.value[mailboxID] || ''
}

function startRowBusy(mailboxID, action) {
  if (!mailboxID || rowBusyAction(mailboxID)) return false
  rowBusyActions.value = { ...rowBusyActions.value, [mailboxID]: action }
  return true
}

function finishRowBusy(mailboxID, action) {
  if (rowBusyAction(mailboxID) !== action) return
  const next = { ...rowBusyActions.value }
  delete next[mailboxID]
  rowBusyActions.value = next
}

function startRowCodeBusy(mailboxID) {
  if (!startRowBusy(mailboxID, 'code')) return false
  const rowKey = `code-row:${mailboxID}`
  codeBusyVisible.value = ''
  clearTimeout(codeBusyTimer)
  codeBusyTimer = window.setTimeout(() => {
    if (rowBusyAction(mailboxID) === 'code') codeBusyVisible.value = rowKey
  }, 600)
  return true
}

function finishRowCodeBusy(mailboxID) {
  clearTimeout(codeBusyTimer)
  finishRowBusy(mailboxID, 'code')
  if (codeBusyVisible.value === `code-row:${mailboxID}`) codeBusyVisible.value = ''
}

function formatTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) || date.getFullYear() < 2000 ? '-' : date.toLocaleString('zh-CN')
}

function formatMessageTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime()) || date.getFullYear() < 2000) return '-'
  const today = new Date()
  const sameDay = date.getFullYear() === today.getFullYear() && date.getMonth() === today.getMonth() && date.getDate() === today.getDate()
  return sameDay
    ? date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })
    : date.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

function looksLikeHTML(value) {
  return /<(?:!doctype|html|head|body|style|table|div|p|a|img|span)\b/i.test(String(value || ''))
}

function messageContentTypeLabel(message) {
  const contentType = String(message?.content_type || '').toLowerCase()
  if (String(message?.html_body || '').trim() || contentType.includes('text/html') || looksLikeHTML(message?.body)) return 'HTML 邮件'
  if (contentType.includes('text/plain')) return '纯文本邮件'
  return '邮件正文'
}

function shrinkEmailFontSizes(value) {
  return String(value || '').replace(/(font-size\s*:\s*)(\d+(?:\.\d+)?)px/gi, (match, prefix, rawSize) => {
    const size = Number(rawSize)
    if (!Number.isFinite(size) || size < 12) return match
    const reducedSize = Math.max(11, Math.round(size * 0.82))
    return `${prefix}${reducedSize}px`
  })
}

function buildEmailHTMLDocument(value) {
  if (!value || typeof DOMParser === 'undefined') return ''
  const documentNode = new DOMParser().parseFromString(value, 'text/html')
  documentNode.querySelectorAll('script, iframe, object, embed, form, input, button, textarea, select, base, meta[http-equiv="refresh"]').forEach((element) => element.remove())
  documentNode.querySelectorAll('*').forEach((element) => {
    for (const attribute of [...element.attributes]) {
      const name = attribute.name.toLowerCase()
      const attributeValue = attribute.value.trim().toLowerCase()
      if (name.startsWith('on') || ((name === 'href' || name === 'src' || name === 'action') && attributeValue.startsWith('javascript:'))) {
        element.removeAttribute(attribute.name)
      }
    }
  })
  documentNode.querySelectorAll('a[href]').forEach((link) => {
    link.setAttribute('target', '_blank')
    link.setAttribute('rel', 'noopener noreferrer')
  })
  // 只调整用于展示的副本，保留数据库中的原始邮件 HTML。
  documentNode.querySelectorAll('style').forEach((element) => {
    element.textContent = shrinkEmailFontSizes(element.textContent)
  })
  documentNode.querySelectorAll('[style]').forEach((element) => {
    element.setAttribute('style', shrinkEmailFontSizes(element.getAttribute('style')))
  })
  const policy = documentNode.createElement('meta')
  policy.setAttribute('http-equiv', 'Content-Security-Policy')
  policy.setAttribute('content', "default-src 'none'; img-src https: http: data:; style-src 'unsafe-inline' https: http:; font-src https: http: data:; media-src https: http: data:; script-src 'none'; object-src 'none'; frame-src 'none'; form-action 'none'")
  const viewport = documentNode.createElement('meta')
  viewport.setAttribute('name', 'viewport')
  viewport.setAttribute('content', 'width=device-width, initial-scale=1')
  const readerStyle = documentNode.createElement('style')
  readerStyle.textContent = 'html{color-scheme:light;background:#fff;scrollbar-width:thin;scrollbar-color:#cbd5e1 transparent}body{box-sizing:border-box;min-height:100%;margin:0;padding:24px;color:#172033;background:#fff;font-size:13px;overflow-wrap:anywhere}::-webkit-scrollbar{width:8px;height:8px}::-webkit-scrollbar-track{background:transparent}::-webkit-scrollbar-thumb{border:2px solid transparent;border-radius:9999px;background:#cbd5e1;background-clip:content-box}img{max-width:100%;height:auto}table{max-width:100%}pre{white-space:pre-wrap}a{color:#0a6cff}'
  documentNode.head.prepend(policy, viewport, readerStyle)
  return `<!doctype html>\n${documentNode.documentElement.outerHTML}`
}

async function openMessage(item) {
  if (!item || !selected.value?.id) return
  const requestID = ++messageLoadRequestID
  selectedMessage.value = item
  messageViewMode.value = selectedMessageHasHTML.value ? 'html' : 'text'
  selectedMessageLoading.value = true
  try {
    const data = await api(`/api/mailboxes/${selected.value.id}/messages/${item.id}`)
    if (requestID !== messageLoadRequestID || selectedMessage.value?.id !== item.id) return
    const detail = data.message || item
    selectedMessage.value = detail
    messageViewMode.value = selectedMessageHasHTML.value ? 'html' : 'text'
    messages.value = messages.value.map((message) => message.id === detail.id ? detail : message)
  } catch (err) {
    if (requestID === messageLoadRequestID && selectedMessage.value?.id === item.id) {
      flash(`完整邮件加载失败：${err.message}`, true)
    }
  } finally {
    if (requestID === messageLoadRequestID) selectedMessageLoading.value = false
  }
}

function closeSelectedMessage() {
  messageLoadRequestID++
  selectedMessageLoading.value = false
  selectedMessage.value = null
  messageViewMode.value = 'auto'
}

function cleanupText(cleanup) {
  const moved = cleanup?.moved_to_trash || 0
  const destroyed = cleanup?.destroyed || 0
  const skipped = cleanup?.skipped || 0
  const localRemoved = cleanup?.local_removed || 0
  return `移动 ${moved} 封，彻底清除 ${destroyed} 封${localRemoved ? `，本地清理 ${localRemoved} 封` : ''}${skipped ? `，未匹配 ${skipped} 封` : ''}`
}

function appleMailFolderText(value) {
  const name = String(value || '').trim()
  if (!name) return ''
  const normalized = name.toLowerCase()
  const categoryMarker = '$category$_'
  const categoryIndex = normalized.indexOf(categoryMarker)
  if (categoryIndex >= 0) {
    let category = normalized.slice(categoryIndex + categoryMarker.length)
    const highlighted = category.endsWith('_hi')
    if (highlighted) category = category.slice(0, -3)
    let label = ({ primary: '主要', decluttered: '智能整理', personal: '个人', transactions: '交易', updates: '更新', news: '新闻', social: '社交', others: '其他', promotions: '推广', error: '分类异常', unsupportedlanguage: '不支持的语言' })[category] || '智能分类'
    if (highlighted) label += '·重点'
    return `收件箱（${label}）`
  }
  return ({
    inbox: '收件箱',
    sent: '已发送',
    'sent mail': '已发送',
    'sent messages': '已发送',
    drafts: '草稿箱',
    archive: '归档',
    junk: '垃圾邮件',
    'junk mail': '垃圾邮件',
    'bulk mail': '垃圾邮件',
    spam: '垃圾邮件',
    trash: '废纸篓',
    deleted: '废纸篓',
    'deleted messages': '废纸篓',
    'all mail': '所有邮件',
  })[normalized] || name
}

function appleMailCleanupText(job) {
  const total = Number(job?.total_accounts || 0)
  const totalMailboxes = Number(job?.total_mailboxes || 0)
  const active = Number(job?.active || 0)
  const queued = Number(job?.queued || 0)
  const completedAccounts = Number(job?.completed || 0)
  const successfulAccounts = Number(job?.success || 0)
  const failedAccounts = Number(job?.failed || 0)
  const completedMailboxes = Number(job?.completed_mailboxes || 0)
  const successfulMailboxes = Number(job?.successful_mailboxes || 0)
  const failedMailboxes = Number(job?.failed_mailboxes || 0)
  const folder = job?.current_folder ? `｜当前文件夹 ${appleMailFolderText(job.current_folder)}` : ''
  const progress = totalMailboxes > 0
    ? `邮箱已完成 ${completedMailboxes}/${totalMailboxes}（成功 ${successfulMailboxes}，失败 ${failedMailboxes}）`
    : `账号已完成 ${completedAccounts}/${total}（成功 ${successfulAccounts}，失败 ${failedAccounts}）`
  const counts = `发现邮件 ${job?.discovered || 0}｜移入废纸篓 ${job?.moved_to_trash || 0}｜彻底删除 ${job?.destroyed || 0}｜本地清理 ${job?.local_removed || 0}`
  return `全部邮件清理：Apple 账号 ${total}｜邮箱 ${totalMailboxes}｜执行账号 ${active}｜排队账号 ${queued}｜${progress}${folder}；${counts}`
}

function applyAppleMailCleanupJob(job, showCompleted = true) {
  if (!job || typeof job !== 'object') return
  const wasRunning = Boolean(appleMailCleanup.value.running)
  appleMailCleanup.value = job
  if (job.running) {
    cleanAllNoticeID = updateToast(cleanAllNoticeID, appleMailCleanupText(job), 'info', 0)
    return
  }
  if (!showCompleted && !wasRunning) return
  if (job.status === 'completed') {
    const completedText = Number(job.discovered || 0) > 0 ? '全部 Apple 云端邮件已清理完成' : '未发现需要清理的 Apple 邮件，邮箱状态已确认'
    cleanAllNoticeID = updateToast(cleanAllNoticeID, `${appleMailCleanupText(job)}；${completedText}`, 'success', 7000)
  } else if (job.status === 'partial') {
    cleanAllNoticeID = updateToast(cleanAllNoticeID, `${appleMailCleanupText(job)}；部分账号失败：${job.last_error || '请查看失败账号'}`, 'warning', 9000)
  } else if (job.status === 'cancelled' || job.status === 'interrupted') {
    cleanAllNoticeID = updateToast(cleanAllNoticeID, `全部邮件清理已停止：${job.last_error || '任务未完成'}`, 'warning', 7000)
  }
  if (wasRunning) void load({ silent: true })
}

async function loadAppleMailCleanupStatus() {
  try {
    const data = await api('/api/apple-mail/cleanup/status')
    applyAppleMailCleanupJob(data.job, false)
  } catch {
    return
  }
}

function mailboxMessageSyncJobText(job) {
  const total = Number(job?.total_accounts || 0)
  const completed = Number(job?.completed_accounts || 0)
  const successful = Number(job?.successful_accounts || 0)
  const failed = Number(job?.failed_accounts || 0)
  return `邮件同步：完成 ${completed}/${total}（成功 ${successful}，失败 ${failed}）｜执行 ${job?.active || 0}｜排队 ${job?.queued || 0}｜IMAP ${job?.imap_accounts || 0}｜Web API ${job?.web_api_accounts || 0}｜回退 ${job?.fallbacks || 0}｜扫描 ${job?.scanned || 0}｜匹配 ${job?.matched || 0}｜新增 ${job?.synced_messages || 0}`
}

function mailboxMessageSyncCompletedText(job) {
  const total = Number(job?.total_accounts || 0)
  const successful = Number(job?.successful_accounts || 0)
  const failed = Number(job?.failed_accounts || 0)
  const skipped = Number(job?.skipped_mailboxes || 0) ? `｜跳过邮箱 ${job.skipped_mailboxes}` : ''
  const hasMore = job?.has_more ? '｜仍有后续邮件' : ''
  return `邮件同步完成：账号成功 ${successful}/${total}｜IMAP ${job?.imap_accounts || 0}｜Web API ${job?.web_api_accounts || 0}｜回退 ${job?.fallbacks || 0}｜扫描 ${job?.scanned || 0}｜匹配 ${job?.matched || 0}｜新增 ${job?.synced_messages || 0}｜失败 ${failed}${skipped}${hasMore}`
}

function applyExistingMailboxMessageSyncJob(job, showCompleted = true) {
  if (!job || typeof job !== 'object') return
  const wasRunning = Boolean(existingMailboxMessageSync.value.running)
  existingMailboxMessageSync.value = job
  window.clearTimeout(syncExistingMessagesStatusTimer)
  if (job.running) {
    syncExistingMessagesStatusTimer = window.setTimeout(loadExistingMailboxMessageSyncStatus, 1200)
  }
  if (syncExistingMessagesIntroVisible) return
  if (job.running) {
    syncExistingMessagesNoticeID = updateToast(syncExistingMessagesNoticeID, mailboxMessageSyncJobText(job), 'info', 0)
    return
  }
  if (!showCompleted && !wasRunning) return
  if (job.status === 'completed') {
    const text = Number(job.total_mailboxes || 0) ? mailboxMessageSyncCompletedText(job) : '没有可同步邮件的已有邮箱'
    const type = Number(job.total_mailboxes || 0) && !job.has_more ? 'success' : 'warning'
    syncExistingMessagesNoticeID = updateToast(syncExistingMessagesNoticeID, text, type, 7000)
  } else if (job.status === 'partial') {
    const recentError = job.last_error ? `；最近错误：${job.last_error}` : ''
    syncExistingMessagesNoticeID = updateToast(syncExistingMessagesNoticeID, `${mailboxMessageSyncCompletedText(job)}${recentError}`, job.successful_accounts ? 'warning' : 'error', 9000)
  } else if (job.status === 'interrupted') {
    syncExistingMessagesNoticeID = updateToast(syncExistingMessagesNoticeID, `邮件同步已停止：${job.last_error || '任务未完成'}`, 'warning', 7000)
  }
  if (wasRunning) void load({ silent: true })
}

function showExistingMailboxMessageSyncIntro() {
  window.clearTimeout(syncExistingMessagesIntroTimer)
  if (syncExistingMessagesIntroID !== null) dismissToast(syncExistingMessagesIntroID)
  syncExistingMessagesIntroVisible = true
  syncExistingMessagesIntroID = updateToast(null, '正在读取所有 Apple 主号在保留期以内的邮件：IMAP 主路径，并使用 iCloud Web 补查缺失邮件……', 'info', 1400)
  syncExistingMessagesIntroTimer = window.setTimeout(() => {
    syncExistingMessagesIntroVisible = false
    syncExistingMessagesIntroID = null
    applyExistingMailboxMessageSyncJob(existingMailboxMessageSync.value)
  }, 1450)
}

async function loadExistingMailboxMessageSyncStatus() {
  try {
    const data = await api('/api/mailboxes/sync-messages/status')
    applyExistingMailboxMessageSyncJob(data.job, false)
  } catch {
    return
  }
}

function openSyncDialog() {
  if (!accounts.value.length) {
    flash('请先添加账号（iCloud 账号还需要有效的登录态）', true)
    return
  }
  syncAccountID.value = ''
  showImport.value = false
  showBulkDelete.value = false
  showSync.value = true
}

function openImportDialog() {
  showSync.value = false
  showBulkDelete.value = false
  if (!mailboxImport.account_id && accounts.value.length) mailboxImport.account_id = accounts.value[0].id
  showImport.value = true
}

function openExternalImportDialog() {
  showImport.value = false
  showSync.value = false
  showBulkDelete.value = false
  externalImport.raw = ''
  externalImport.label = ''
  externalImport.note = ''
  showExternalImport.value = true
}

// 抽奖平台发奖文件的固定分隔符：邮箱----取码地址。
const EXTERNAL_PAIR_SEPARATOR = '----'

const externalFileInput = ref(null)

// 把「拼在一起的多个 JSON 文档」切成一个个文档。
// 选中多个奖品文件时它们就是首尾相接的，整段 JSON.parse 会直接失败。
function splitJSONDocuments(text) {
  const documents = []
  let depth = 0
  let start = -1
  let inString = false
  let escaped = false
  for (let i = 0; i < text.length; i += 1) {
    const ch = text[i]
    if (inString) {
      if (escaped) escaped = false
      else if (ch === '\\') escaped = true
      else if (ch === '"') inString = false
      continue
    }
    if (ch === '"') {
      inString = true
    } else if (ch === '{' || ch === '[') {
      if (depth === 0) start = i
      depth += 1
    } else if (ch === '}' || ch === ']') {
      depth -= 1
      if (depth <= 0) {
        if (depth === 0 && start >= 0) documents.push(text.slice(start, i + 1))
        depth = 0
        start = -1
      }
    }
  }
  return documents
}

// 把粘贴文本或奖品文件内容解析成「邮箱 + 取码地址」。
// 后端 ParseExternalMailboxLines 是同一套规则；这里只为让人提交前看清会导入几条。
function extractExternalPairs(raw) {
  const source = String(raw || '').trim()
  if (!source) return []
  let candidates = null
  if (source.startsWith('{') || source.startsWith('[')) {
    const collected = []
    const walk = (value) => {
      if (typeof value === 'string') collected.push(value)
      else if (Array.isArray(value)) value.forEach(walk)
      else if (value && typeof value === 'object') Object.keys(value).sort().forEach((key) => walk(value[key]))
    }
    for (const document of splitJSONDocuments(source)) {
      try {
        walk(JSON.parse(document))
      } catch {
        // 单个文档坏了不影响已经读出来的，和后端 Decoder 的行为一致。
      }
    }
    if (collected.length) candidates = collected
  }
  if (!candidates) candidates = source.split(/\r?\n/)

  const seen = new Set()
  const pairs = []
  for (const candidate of candidates) {
    const text = String(candidate || '').trim()
    if (!text) continue
    let email = ''
    let url = ''
    const separator = text.indexOf(EXTERNAL_PAIR_SEPARATOR)
    if (separator > 0) {
      const left = text.slice(0, separator).trim()
      const right = text.slice(separator + EXTERNAL_PAIR_SEPARATOR.length).trim()
      if (/^[^@\s]+@[^@\s]+\.[A-Za-z]{2,}$/.test(left) && /^https?:\/\/\S+$/.test(right)) {
        email = left
        url = right
      }
    }
    if (!email) {
      // 先把链接整段摘掉再取邮箱，和后端 splitExternalPair 是同一套规则。
      // 否则「邮箱和链接直接粘在一起」的形状（a@b.comhttps://...）里，邮箱正则的
      // 顶级域会把 https 一起吞掉，预览会把它当成一条有效地址。
      url = (text.match(/https?:\/\/[^\s)\]|<>"']+/) || [''])[0]
      const withoutURL = url ? text.replace(url, ' ') : text
      email = (withoutURL.match(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/) || [''])[0]
    }
    if (!email || !url) continue
    const key = email.toLowerCase()
    if (seen.has(key)) continue
    seen.add(key)
    pairs.push({ email: key, url })
  }
  return pairs
}

const externalPreviewCount = computed(() => extractExternalPairs(externalImport.raw).length)

// 读取选中的奖品 JSON 文件，把内容追加进同一个文本框——
// 两条入口共用一套解析规则，不维护第二份逻辑。
async function pickExternalFiles(event) {
  const files = Array.from(event.target.files || [])
  event.target.value = ''
  if (!files.length) return
  const chunks = []
  const unreadable = []
  for (const file of files) {
    try {
      const text = (await file.text()).trim()
      if (text) chunks.push(text)
      else unreadable.push(file.name)
    } catch {
      unreadable.push(file.name)
    }
  }
  if (chunks.length) {
    externalImport.raw = [externalImport.raw.trim(), ...chunks].filter(Boolean).join('\n')
  }
  const parts = [`已读取 ${chunks.length} 个文件`]
  if (unreadable.length) parts.push(`${unreadable.length} 个读取失败：${unreadable.slice(0, 3).join('、')}`)
  flash(parts.join('，'), unreadable.length > 0)
}

// 导入只有取件接口的裸邮箱。没有母号也没有密码，唯一的凭据是取码地址里的令牌。
async function importExternalMailboxes() {
  if (!externalImport.raw.trim()) {
    flash('请粘贴内容或选择奖品 JSON 文件', true)
    return
  }
  if (!externalPreviewCount.value) {
    flash('没能从这些内容里解析出任何「邮箱 + 取码地址」，请检查格式', true)
    return
  }
  if (!startBusy('external-import')) return
  try {
    const payload = await api('/api/mailboxes/import-external', {
      method: 'POST',
      body: JSON.stringify({
        raw: externalImport.raw,
        label: externalImport.label.trim(),
        note: externalImport.note.trim(),
      }),
    })
    const result = payload.result || {}
    const parts = [`新增 ${result.created || 0} 个`, `更新 ${result.updated || 0} 个`]
    if (result.failed) parts.push(`失败 ${result.failed} 个`)
    if (payload.skipped_lines) parts.push(`跳过无法解析的 ${payload.skipped_lines} 条`)
    flash(`外部邮箱导入完成：${parts.join('，')}`, Boolean(result.failed))
    if (!result.failed) showExternalImport.value = false
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('external-import')
  }
}

function parseBulkDeleteEmails(value) {
  const seen = new Set()
  return String(value || '')
    .split(/\r?\n/)
    .map((email) => email.trim().toLowerCase())
    .filter((email) => {
      if (!email || seen.has(email)) return false
      seen.add(email)
      return true
    })
}

function emailListSummary(emails) {
  const items = emails || []
  if (items.length <= 5) return items.join('、')
  return `${items.slice(0, 5).join('、')} 等 ${items.length} 个邮箱`
}

function openBulkDeleteDialog() {
  showImport.value = false
  showSync.value = false
  bulkDeleteEmails.value = ''
  bulkDeleteError.value = ''
  showBulkDelete.value = true
}

function closeBulkDeleteDialog() {
  if (isBusy('bulk-delete-resolve')) return
  showBulkDelete.value = false
  bulkDeleteError.value = ''
}

async function submitBulkDeleteEmails() {
  const emails = parseBulkDeleteEmails(bulkDeleteEmails.value)
  if (!emails.length) {
    bulkDeleteError.value = '请至少输入一个邮箱地址。'
    return
  }
  const invalid = emails.filter((email) => !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email))
  if (invalid.length) {
    bulkDeleteError.value = `邮箱格式不正确：${emailListSummary(invalid)}`
    return
  }
  if (!startBusy('bulk-delete-resolve')) return
  bulkDeleteError.value = ''
  try {
    const data = await api('/api/mailboxes/resolve', {
      method: 'POST',
      body: JSON.stringify({ emails }),
    })
    const resolved = data.items || []
    const targets = resolved.filter((mailbox) => !isMailboxDeleteBusy(mailbox.id))
    const missing = data.missing || []
    if (!targets.length) {
      if (missing.length) bulkDeleteError.value = `本地邮箱池中未找到：${emailListSummary(missing)}`
      else bulkDeleteError.value = '这些邮箱已经在删除队列中。'
      return
    }
    enqueueMailboxDeletions(targets)
    showBulkDelete.value = false
    bulkDeleteEmails.value = ''
    flash(`已将 ${targets.length} 个指定邮箱加入彻底删除队列`)
    if (missing.length) showError(`未在本地邮箱池找到：${emailListSummary(missing)}`)
  } catch (err) {
    bulkDeleteError.value = err.message
  } finally {
    finishBusy('bulk-delete-resolve')
  }
}

function calculateMailboxTableSize() {
  const element = mailboxTableViewport.value
  if (!element) return pageSize.value

  const scrollContainer = element.closest('.page-scroll')
  const scrollTop = scrollContainer?.scrollTop || 0
  const viewportTop = element.getBoundingClientRect().top + scrollTop
  const scrollBottom = scrollContainer?.getBoundingClientRect().bottom || window.innerHeight
  const scrollStyle = scrollContainer ? window.getComputedStyle(scrollContainer) : null
  const bottomPadding = Number.parseFloat(scrollStyle?.paddingBottom || '0') || 0
  const footerHeight = mailboxPagination.value?.getBoundingClientRect().height || 53
  const headerHeight = element.querySelector('thead')?.getBoundingClientRect().height || 36
  const dataRow = element.querySelector('tbody tr:not(.mailbox-empty-row)')
  const rowHeight = dataRow?.getBoundingClientRect().height || 48
  const tableWidth = element.querySelector('table')?.scrollWidth || 0
  const hasHorizontalScrollbar = tableWidth > element.clientWidth + 1
  const reservedHeight = (hasHorizontalScrollbar ? 8 : 0) + 1
  const minimumRows = window.matchMedia('(max-width: 620px)').matches ? 3 : 5
  const availableHeight = scrollBottom - viewportTop - footerHeight - bottomPadding - 2
  const visibleRows = Math.max(minimumRows, Math.min(50, Math.floor((availableHeight - headerHeight - reservedHeight) / rowHeight)))

  mailboxEmptyHeight.value = Math.floor(visibleRows * rowHeight)
  mailboxTableHeight.value = Math.floor(headerHeight + mailboxEmptyHeight.value + reservedHeight)
  return visibleRows
}

function applyMailboxTableSize(shouldReload = true) {
  const nextPageSize = calculateMailboxTableSize()
  if (nextPageSize === pageSize.value) return false

  const firstVisibleIndex = (page.value - 1) * pageSize.value
  pageSize.value = nextPageSize
  page.value = Math.floor(firstVisibleIndex / nextPageSize) + 1
  if (shouldReload) load()
  return true
}

function scheduleMailboxTableSize() {
  window.clearTimeout(tableResizeTimer)
  tableResizeTimer = window.setTimeout(() => applyMailboxTableSize(), 100)
}

async function load(options = {}) {
  const silent = Boolean(options.silent)
  const requestID = ++loadRequestID
  if (!silent) {
    loading.value = true
    loadingVisible.value = false
    clearTimeout(loadingTimer)
    loadingTimer = setTimeout(() => {
      if (loading.value && requestID === loadRequestID) loadingVisible.value = true
    }, 600)
  }
  try {
    const params = new URLSearchParams({ page: String(page.value), page_size: String(pageSize.value) })
    if (query.value.trim()) params.set('q', query.value.trim())
    if (accountID.value) params.set('account_id', accountID.value)
    if (status.value) params.set('status', status.value)
    const data = await api(`/api/mailboxes?${params}`)
    if (requestID !== loadRequestID) return
    result.value = data
    page.value = result.value.page
  } catch (err) {
    if (!silent && requestID === loadRequestID) flash(err.message, true)
  } finally {
    if (!silent && requestID === loadRequestID) {
      clearTimeout(loadingTimer)
      loading.value = false
      loadingVisible.value = false
      await nextTick()
      scheduleMailboxTableSize()
    }
  }
}

async function refreshMailboxPool() {
  if (document.hidden || loading.value || autoRefreshing || deleteQueueRunning || busyActions.value.length || hasRowBusyActions.value) return
  autoRefreshing = true
  try {
    await load({ silent: true })
    const selectedID = selected.value?.id
    if (!selectedID || quickEditOpen.value) return
    const [detail, messageData] = await Promise.all([
      api(`/api/mailboxes/${selectedID}`),
      api(`/api/mailboxes/${selectedID}/messages`),
    ])
    if (selected.value?.id !== selectedID) return
    const previous = selected.value
    if (edit.status === previous.status) edit.status = detail.mailbox.status
    if (edit.api_active === previous.api_active) edit.api_active = detail.mailbox.api_active
    if (edit.icloud_active === previous.icloud_active) edit.icloud_active = detail.mailbox.icloud_active
    if (edit.note === (previous.note || '')) edit.note = detail.mailbox.note || ''
    selected.value = detail.mailbox
    messages.value = messageData.items || []
  } catch {
    return
  } finally {
    autoRefreshing = false
  }
}

// 并行拉取四个来源的账号并合并。
// 单个接口失败（例如某个模块被关掉）不能清空整个列表，所以逐个兜住错误，
// 只有全部失败才提示。
// 每页按 200 拉并翻到拿完：IMAP 母号有几百个，一页 20 条的话账号筛选只能看到
// 头 20 个，其余账号名下的邮箱照样显示「账号未加载」（后端单页上限就是 200）。
async function loadAccounts() {
  const merged = []
  let failed = 0
  await Promise.all(ACCOUNT_ENDPOINTS.map(async ([provider, path]) => {
    try {
      let page = 1
      for (;;) {
        const data = await api(`${path}${path.includes('?') ? '&' : '?'}page_size=200&page=${page}`)
        for (const item of data.items || []) merged.push({ ...item, provider: item.provider || provider })
        const totalPages = Number(data.total_pages || 1)
        if (page >= totalPages || !(data.items || []).length) break
        page += 1
      }
    } catch {
      failed += 1
    }
  }))
  merged.sort((left, right) => {
    const order = (value) => ACCOUNT_ENDPOINTS.findIndex(([provider]) => provider === (value?.provider || 'icloud'))
    const delta = order(left) - order(right)
    return delta !== 0 ? delta : accountName(left).localeCompare(accountName(right), 'zh-Hans-CN')
  })
  accounts.value = merged
  if (failed === ACCOUNT_ENDPOINTS.length) {
    flash('读取账号列表失败，请检查各服务商的授权状态', true)
  }
}

function scheduleRealtimeMailboxRefresh(change) {
	if (applyRealtimeMailboxChange(change)) return
  pendingRealtimeResources.add(change.resource)
  window.clearTimeout(realtimeRefreshTimer)
  realtimeRefreshTimer = window.setTimeout(() => {
    const resources = new Set(pendingRealtimeResources)
    pendingRealtimeResources.clear()
    if (resources.has('apple-account')) void loadAccounts()
    void refreshMailboxPool()
  }, 120)
}

function applyRealtimeMailboxChange(change) {
  const payload = change?.payload || {}
	if (change.resource === 'apple-mail-cleanup' && payload.data) {
		applyAppleMailCleanupJob(payload.data)
		return true
	}
	if (change.resource === 'mailbox-message-sync' && payload.data) {
		applyExistingMailboxMessageSyncJob(payload.data)
		return true
	}
	if (change.resource === 'mailbox-lease') return true
  if (change.resource === 'mailbox' && payload.operation === 'batch-updated' && Array.isArray(payload.items)) {
    const updates = new Map(payload.items.map((item) => [item.id, item]))
    result.value.items = (result.value.items || []).map((item) => updates.has(item.id) ? { ...item, ...updates.get(item.id) } : item)
    if (selected.value && updates.has(selected.value.id)) selected.value = { ...selected.value, ...updates.get(selected.value.id) }
    if (selected.value && Array.isArray(payload.messages)) {
      const incoming = payload.messages.filter((item) => item.mailbox_id === selected.value.id)
      if (incoming.length) {
        const merged = new Map(messages.value.map((item) => [item.id, item]))
        incoming.forEach((item) => merged.set(item.id, item))
        messages.value = [...merged.values()].sort((left, right) => new Date(right.received_at) - new Date(left.received_at))
      }
    }
    return true
  }
  if (change.resource === 'mailbox' && payload.data?.id && change.operation === 'updated') {
    let applied = false
    result.value.items = (result.value.items || []).map((item) => {
      if (item.id !== payload.data.id) return item
      applied = true
      return { ...item, ...payload.data }
    })
    if (selected.value?.id === payload.data.id) {
      selected.value = { ...selected.value, ...payload.data }
      applied = true
    }
    return applied
  }
  if (change.resource === 'message' && change.operation === 'created' && payload.data?.mailbox_id === selected.value?.id) {
    if (!messages.value.some((item) => item.id === payload.data.id)) messages.value = [payload.data, ...messages.value]
    return true
  }
  if (change.resource === 'apple-account' && payload.data?.id && change.operation === 'updated') {
    let applied = false
    accounts.value = accounts.value.map((item) => {
      if (item.id !== payload.data.id) return item
      applied = true
      return { ...item, ...payload.data }
    })
    return applied
  }
  return false
}

async function importMailbox() {
  if (!mailboxImport.account_id || !mailboxImport.email.trim()) return
  if (!startBusy('import')) return
  try {
    const data = await api('/api/mailboxes', { method: 'POST', body: JSON.stringify(mailboxImport) })
    flash(data.created ? `已导入 ${data.mailbox.email}` : `${data.mailbox.email} 已存在，已更新绑定信息`)
    Object.assign(mailboxImport, { account_id: mailboxImport.account_id, email: '', label: '', note: '' })
    showImport.value = false
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('import')
  }
}

// 「同步已有邮箱」按账号所属服务商走各自的接口。
// iCloud 是 Hide My Email 的同步接口，微软和谷歌是别名导入接口；
// 两者返回的统计字段不同，这里统一折算成「发现多少个地址」。
function providerSyncPath(account) {
  const provider = account?.provider || 'icloud'
  if (provider === 'microsoft') return `/api/microsoft-accounts/${account.id}/aliases/import`
  if (provider === 'google') return `/api/google-accounts/${account.id}/aliases/import`
  return `/api/apple-accounts/${account.id}/mailboxes/sync`
}

function syncResultCount(data) {
  if (!data) return 0
  if (typeof data.count === 'number') return data.count
  if (Array.isArray(data.items)) return data.items.length
  if (typeof data.found === 'number') return data.found
  return (Number(data.created) || 0) + (Number(data.updated) || 0)
}

async function syncExistingMailboxes() {
  if (isBusy('sync-existing')) return
  const targets = syncAccountID.value
    ? accounts.value.filter((item) => item.id === syncAccountID.value)
    : accounts.value
  if (!targets.length) {
    flash('没有可同步的账号，请先添加账号', true)
    return
  }
  startBusy('sync-existing')
  const failures = []
  let total = 0
  let succeeded = 0
  try {
    for (const [index, account] of targets.entries()) {
      const finished = succeeded + failures.length
      syncExistingNoticeID = updateToast(
        syncExistingNoticeID,
        `同步已有邮箱：执行中 1｜排队中 ${Math.max(0, targets.length - index - 1)}｜已完成 ${finished}/${targets.length}（成功 ${succeeded}，失败 ${failures.length}）`,
        'info',
        0,
      )
      try {
        const data = await api(providerSyncPath(account), { method: 'POST', body: '{}' })
        total += syncResultCount(data)
        succeeded += 1
      } catch (err) {
        failures.push(`${accountName(account)}：${err.message}`)
      }
    }
    showSync.value = false
    page.value = 1
    await load()
    const noticeType = failures.length ? (succeeded ? 'warning' : 'error') : 'success'
    const noticeText = failures.length
      ? `同步已有邮箱已结束：成功 ${succeeded}｜失败 ${failures.length}${failures.length ? `；最近错误：${failures.at(-1)}` : ''}`
      : `同步已有邮箱已完成：成功 ${succeeded}｜失败 0`
    syncExistingNoticeID = updateToast(syncExistingNoticeID, noticeText, noticeType, 7000)
    if (failures.length && succeeded) {
      flash(`同步完成 ${succeeded} 个账号，共发现 ${total} 个已有邮箱；${failures.join('；')}`, true)
    } else if (failures.length) {
      flash(`同步已有邮箱失败：${failures.join('；')}`, true)
    } else {
      flash(`同步完成：${succeeded} 个账号，共发现 ${total} 个已有邮箱`)
    }
  } catch (err) {
    syncExistingNoticeID = updateToast(syncExistingNoticeID, `同步已有邮箱失败：${err.message}`, 'error', 7000)
  } finally {
    finishBusy('sync-existing')
  }
}

async function syncExistingMailboxMessages() {
  if (isBusy('sync-existing-messages') || isBusy('sync-existing') || existingMailboxMessageSync.value.running) return
  if (!startBusy('sync-existing-messages')) return
  showExistingMailboxMessageSyncIntro()
  try {
    const data = await api('/api/mailboxes/sync-messages', { method: 'POST', body: '{}' })
    applyExistingMailboxMessageSyncJob(data.job)
  } catch (err) {
    syncExistingMessagesIntroVisible = false
    window.clearTimeout(syncExistingMessagesIntroTimer)
    if (syncExistingMessagesIntroID !== null) dismissToast(syncExistingMessagesIntroID)
    syncExistingMessagesIntroID = null
    if (err.code === 'mailbox_message_sync_running') {
      await loadExistingMailboxMessageSyncStatus()
    } else {
      syncExistingMessagesNoticeID = updateToast(syncExistingMessagesNoticeID, `邮件同步启动失败：${err.message}`, 'error', 7000)
    }
  } finally {
    finishBusy('sync-existing-messages')
  }
}

// 体检池里的每个邮箱还能不能读信，并把结论落到邮箱自己的状态上：判定为凭据失效的会被
// 标成失败、从池里摘掉。不发信、不取邮件正文；网络类的失败一律不改状态。
//
// 全池逐个连服务器，几十个邮箱要跑上一两分钟，所以按小批循环发请求——和「同步已有邮箱」
// 同一个约定：一个长请求会被浏览器掐断，而且这期间界面上什么都看不到。分批之后每一批
// 的进度都能立刻显示出来。
async function runMailboxHealthCheck() {
  if (!startBusy('health-check')) return
  healthCheck.value = { running: true, done: 0, total: 0, report: null }
  healthCheckOpen.value = true
  try {
    // 范围是整个邮箱池，不只是当前这一页：只体检看得见的几条，等于把「看不见的那些
    // 已经坏了」一直藏下去。
    const ids = []
    for (let pageIndex = 1; ; pageIndex += 1) {
      const data = await api(`/api/mailboxes?page=${pageIndex}&page_size=200`)
      ids.push(...(data.items || []).map((item) => item.id))
      if (pageIndex >= (data.total_pages || 1)) break
    }
    if (!ids.length) {
      healthCheck.value = { running: false, done: 0, total: 0, report: null }
      healthCheckOpen.value = false
      flash('邮箱池里还没有邮箱', true)
      return
    }
    healthCheck.value = { running: true, done: 0, total: ids.length, report: null }
    const items = []
    for (let start = 0; start < ids.length; start += 20) {
      const batch = ids.slice(start, start + 20)
      const data = await api('/api/mailboxes/probe-batch', { method: 'POST', body: JSON.stringify({ ids: batch }) })
      items.push(...(data.items || []))
      healthCheck.value = { ...healthCheck.value, done: items.length }
    }
    // 接口返回的每个计数都只覆盖它那一个小批，所以汇总必须在合并之后重算：
    // 直接拿最后一批的数字当全池的数字，正是这类界面最容易说出口的一句假话。
    const report = { items, checked: 0, readable: 0, unreadable: 0, not_covered: 0, missing: 0, never_checked: 0, unreadable_but_offered: 0, demoted: 0, account_status_changed: 0 }
    for (const item of items) {
      if (item.missing) report.missing += 1
      else if (!item.covered) report.not_covered += 1
      else if (item.readable) { report.checked += 1; report.readable += 1 }
      else { report.checked += 1; report.unreadable += 1 }
      if (item.never_checked) report.never_checked += 1
      if (item.offered_while_unreadable) report.unreadable_but_offered += 1
      if (item.demoted) report.demoted += 1
      if (item.account_status) report.account_status_changed += 1
    }
    // 合并后重排：「读不了的排最前」这句话得对整张表成立，而不只是对每一批成立。
    report.items = [...items].sort((a, b) => healthCheckRank(a) - healthCheckRank(b))
    healthCheck.value = { running: false, done: ids.length, total: ids.length, report }
    flash(report.unreadable
      ? `体检完成：${report.unreadable} 个读不了，已摘掉 ${report.demoted} 个，另有 ${report.account_status_changed} 个账号状态已更新`
      : `体检完成：${report.readable} 个能读，没有发现读不了的`, Boolean(report.unreadable))
  } catch (err) {
    healthCheck.value = { running: false, done: 0, total: 0, report: null }
    healthCheckOpen.value = false
    flash(`体检失败：${err.message}`, true)
  } finally {
    finishBusy('health-check')
  }
}

function healthCheckRank(item) {
  if (item.covered && !item.readable) return 0
  if (item.missing || !item.covered) return 1
  return 2
}

// 措辞必须和 IMAP 账号页上的徽章一致，否则报告说「连不上」而徽章显示 unreachable，对不上。
function accountStatusLabel(item) {
  return ({ active: '正常', partial: '部分失败', failed: '失败', unreachable: '连不上' })[item.account_status] || item.account_status
}

function closeHealthCheck() {
  healthCheckOpen.value = false
  healthCheck.value = { running: false, done: 0, total: 0, report: null }
}

async function openMailbox(mailbox) {
  if (!startRowBusy(mailbox.id, 'detail')) return
  code.value = null
  codeMailbox.value = null
  codeError.value = ''
  codeDialogOpen.value = false
  selectedMessage.value = null
  try {
    const [detail, messageData] = await Promise.all([
      api(`/api/mailboxes/${mailbox.id}`),
      api(`/api/mailboxes/${mailbox.id}/messages`),
    ])
    selected.value = detail.mailbox
    messages.value = messageData.items || []
    Object.assign(edit, {
      status: selected.value.status,
      api_active: selected.value.api_active,
      icloud_active: selected.value.icloud_active,
      note: selected.value.note || '',
    })
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishRowBusy(mailbox.id, 'detail')
  }
}

async function saveStatus() {
  if (!selected.value) return
  if (!startBusy('save')) return
  try {
    const data = await api(`/api/mailboxes/${selected.value.id}/status`, { method: 'POST', body: JSON.stringify(edit) })
    selected.value = data.mailbox
    flash('邮箱状态已保存')
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('save')
  }
}

function beginMailboxSync() {
  if (!mailboxSyncBatch) mailboxSyncBatch = { total: 0, running: 0, success: 0, failed: 0, noticeID: null }
  mailboxSyncBatch.total += 1
  mailboxSyncBatch.running += 1
  mailboxSyncBatch.noticeID = updateToast(
    mailboxSyncBatch.noticeID,
    `邮件同步：执行中 ${mailboxSyncBatch.running}｜排队中 0｜已完成 ${mailboxSyncBatch.success + mailboxSyncBatch.failed}/${mailboxSyncBatch.total}（成功 ${mailboxSyncBatch.success}，失败 ${mailboxSyncBatch.failed}）`,
    'info',
    0,
  )
  return mailboxSyncBatch
}

function finishMailboxSync(batch, successful) {
  if (mailboxSyncBatch !== batch) return
  batch.running = Math.max(0, batch.running - 1)
  if (successful) batch.success += 1
  else batch.failed += 1
  if (batch.running > 0) {
    batch.noticeID = updateToast(
      batch.noticeID,
      `邮件同步：执行中 ${batch.running}｜排队中 0｜已完成 ${batch.success + batch.failed}/${batch.total}（成功 ${batch.success}，失败 ${batch.failed}）`,
      'info',
      0,
    )
    return
  }
  const type = batch.failed ? (batch.success ? 'warning' : 'error') : 'success'
  batch.noticeID = updateToast(batch.noticeID, `邮件同步已完成：成功 ${batch.success}｜失败 ${batch.failed}`, type, 7000)
  mailboxSyncBatch = null
}

function mailboxMessageSyncSummary(data) {
  return `IMAP ${data.imap_accounts || 0}｜Web API ${data.web_api_accounts || 0}｜扫描 ${data.scanned || 0}｜匹配 ${data.matched || 0}｜新增 ${data.synced_messages || data.synced || 0}`
}

async function runMailboxMessageSync(mailbox, { detail = false } = {}) {
  if (!mailbox) return
  const started = detail ? startBusy('sync') : startRowBusy(mailbox.id, 'sync')
  if (!started) return
  const batch = beginMailboxSync()
  let successful = false
  try {
    const data = await api(`/api/mailboxes/${mailbox.id}/sync`, { method: 'POST' })
    successful = true
    flash(`同步完成：${mailboxMessageSyncSummary(data)}`)
    if (detail) await openMailbox(mailbox)
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishMailboxSync(batch, successful)
    if (detail) finishBusy('sync')
    else finishRowBusy(mailbox.id, 'sync')
  }
}

async function syncMailbox() {
  await runMailboxMessageSync(selected.value, { detail: true })
}

// 回填历史：只作用于这一个邮箱，取回它已经在服务器上的旧邮件。
// 与「同步邮件」的区别是它不看游标、也不看最近 24 小时——那两条正是旧邮件拿不到的原因。
// 邮件多时耗时较长，所以它只在这一个邮箱上跑，并如实把扫描量和新增量报出来。
async function backfillMailbox() {
  if (!selected.value) return
  if (!startBusy('backfill')) return
  const target = selected.value
  try {
    const data = await api(`/api/mailboxes/${target.id}/backfill`, { method: 'POST' })
    flash(`回填完成：${mailboxMessageSyncSummary(data)}`)
    await openMailbox(target)
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('backfill')
  }
}

async function quickSyncMailbox(mailbox) {
  await runMailboxMessageSync(mailbox)
}

async function getCode() {
  if (!selected.value) return
  openCodeDialog(selected.value)
  startCodeBusy('code')
  try {
    code.value = await api(`/api/mailboxes/${selected.value.id}/code?allow_stale=1`)
    const [detail, messageData] = await Promise.all([
      api(`/api/mailboxes/${selected.value.id}`),
      api(`/api/mailboxes/${selected.value.id}/messages`),
    ])
    selected.value = detail.mailbox
    codeMailbox.value = detail.mailbox
    messages.value = messageData.items || []
    await load()
    flash(`已提取验证码 ${code.value.code}`)
  } catch (err) {
    codeError.value = err.message
    flash(err.message, true)
  } finally {
    finishCodeBusy('code')
  }
}

async function quickGetCode(mailbox) {
  if (!startRowCodeBusy(mailbox.id)) return
  openCodeDialog(mailbox)
  try {
    code.value = await api(`/api/mailboxes/${mailbox.id}/code?allow_stale=1`)
    const detail = await api(`/api/mailboxes/${mailbox.id}`)
    codeMailbox.value = detail.mailbox
    await load()
    flash(`已提取验证码 ${code.value.code}`)
  } catch (err) {
    codeError.value = err.message
    flash(err.message, true)
  } finally {
    finishRowCodeBusy(mailbox.id)
  }
}

function openCodeDialog(mailbox) {
  code.value = null
  codeMailbox.value = { ...mailbox }
  codeError.value = ''
  codeDialogOpen.value = true
}

function closeCodeDialog() {
  if (codeDialogBusy.value) return
  codeDialogOpen.value = false
  code.value = null
  codeMailbox.value = null
  codeError.value = ''
}

async function copyCode() {
  if (!code.value?.code) return
  try {
    await copyText(code.value.code)
    flash('验证码已复制')
  } catch (err) {
    flash(err.message || '复制验证码失败，请重试', true)
  }
}

function openQuickEdit(mailbox, field) {
  quickEditMailbox.value = mailbox
  quickEditField.value = field
  quickEdit.status = mailbox.status || 'available'
  quickEdit.note = mailbox.note || ''
  quickEditOpen.value = true
}

function closeQuickEdit() {
  if (isBusy('quick-edit')) return
  quickEditOpen.value = false
  quickEditMailbox.value = null
}

async function saveQuickEdit() {
  if (!quickEditMailbox.value) return
  const field = quickEditField.value
  const payload = field === 'note'
    ? { note: quickEdit.note }
    : { status: quickEdit.status }
  if (!startBusy('quick-edit')) return
  try {
    await api(`/api/mailboxes/${quickEditMailbox.value.id}/status`, { method: 'POST', body: JSON.stringify(payload) })
    flash(field === 'note' ? '邮箱备注已保存' : '邮箱状态已保存')
    quickEditOpen.value = false
    quickEditMailbox.value = null
    await load()
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('quick-edit')
  }
}

async function cleanRemote() {
  if (!selected.value || (!remoteClean.move_synced && !remoteClean.empty_trash)) return
  const actions = [remoteClean.move_synced ? '把已同步邮件移入废纸篓' : '', remoteClean.empty_trash ? '清空该账号废纸篓' : ''].filter(Boolean).join('，并')
  const confirmed = await confirmAction({
    title: '清理 Apple 远端邮件',
    message: `将${actions}，这项操作会修改 Apple 服务器上的邮件。`,
    confirmText: '确认清理',
    tone: 'danger',
  })
  if (!confirmed) return
  if (!startBusy('clean')) return
  try {
    const data = await api(`/api/mailboxes/${selected.value.id}/remote-clean`, { method: 'POST', body: JSON.stringify(remoteClean) })
    await openMailbox(selected.value)
    await load()
    flash(`远端清理完成：${cleanupText(data.cleanup)}`)
  } catch (err) {
    flash(err.message, true)
  } finally {
    finishBusy('clean')
  }
}

async function cleanAllAppleMail() {
  if (isBusy('clean-summary') || isBusy('clean-start') || appleMailCleanup.value.running) return
  startBusy('clean-summary')
  try {
    const dashboard = await api('/api/dashboard')
    const confirmed = await confirmAction({
      title: '全部彻底清理 Apple 邮件',
      message: `清理范围：Apple 账号 ${dashboard.apple_account_count || 0} 个，本地邮件 ${dashboard.message_count || 0} 封。将逐个扫描每个账号的收件箱、已发送、草稿、归档、垃圾邮件和自定义文件夹，把全部 Apple 云端邮件移入废纸篓后彻底删除，再清理本地邮件数据。隐私邮箱地址本身会保留，此操作不可恢复。`,
      confirmText: '确认全部清理',
      tone: 'danger',
    })
    if (!confirmed) return
    finishBusy('clean-summary')
    startBusy('clean-start')
    const data = await api('/api/apple-mail/cleanup', {
      method: 'POST',
      body: JSON.stringify({ account_ids: [], scope: 'all', strategy: 'move_then_destroy', purge_local: true }),
    })
    applyAppleMailCleanupJob(data.job)
  } catch (err) {
    if (cleanAllNoticeID !== null) cleanAllNoticeID = updateToast(cleanAllNoticeID, `全部邮件清理失败：${err.message}`, 'error', 7000)
    else flash(err.message, true)
  } finally {
    finishBusy('clean-summary')
    finishBusy('clean-start')
  }
}

async function removeSelectedMailboxes() {
  const plan = selectedDeletePlan.value
  const targets = [...plan.remote, ...plan.local]
  if (!targets.length) {
    flash(plan.blocked.length ? '选中的都是主地址。主地址就是那一行账号本身，删掉它的池子记录会让寄到主地址的信被丢弃，所以不提供删除' : '选中的邮箱都已在删除队列中', true)
    return
  }
  if (deleteConfirmID.value) {
    flash('请先完成当前删除确认', true)
    return
  }
  deleteConfirmID.value = 'selected'
  try {
    const parts = []
    if (plan.remote.length) parts.push(`${plan.remote.length} 个彻底删除：按本地已同步邮件保存的远端标识，把对应 Apple 邮件移入废纸篓并清空所属账号的整个废纸篓，再删除 Apple 隐私邮箱和本地记录`)
    if (plan.local.length) parts.push(`${plan.local.length} 个只删本地：这类邮箱没有 Apple 远端可清理，会先清空本地邮件再删掉本地记录，Apple 服务器上的隐私邮箱保留`)
    const excluded = plan.blocked.length ? `　选中的 ${plan.blocked.length} 个主地址不会被执行：删掉它们所属的池子记录，寄到主地址的信会因为匹配不到任何本地地址而被丢弃。` : ''
    const confirmed = await confirmAction({
      title: `删除选中的 ${targets.length} 个邮箱`,
      message: `按每个邮箱自己所属的类别分别处理——${parts.join('；')}。未同步到本地的历史邮件不会参与定位，此操作不可恢复。${excluded}`,
      confirmText: '确认删除',
      tone: 'danger',
    })
    if (!confirmed) return
    // localOnly 逐项给，不整批共用一个：一批里可以同时有走远端和只删本地的两种。
    enqueueMailboxDeletions([
      ...plan.remote.map((mailbox) => ({ ...mailbox, localOnly: false })),
      ...plan.local.map((mailbox) => ({ ...mailbox, localOnly: true })),
    ])
  } finally {
    deleteConfirmID.value = ''
  }
}

function enqueueMailboxDeletions(mailboxes) {
  const targets = (mailboxes || []).filter((mailbox) => mailbox?.id && !isMailboxDeleting(mailbox.id) && !isMailboxDeleteQueued(mailbox.id))
  if (!targets.length) return
  if (!deleteQueueRunning) {
    deleteSucceeded = 0
    deleteFailed = 0
    deleteLastError = ''
  }
  deleteQueue.value.push(...targets.map((mailbox) => ({
    id: mailbox.id,
    email: mailbox.email,
    accountID: mailbox.account_id || mailbox.accountID || '',
    localOnly: Boolean(mailbox.localOnly),
  })))
  updateDeleteProgress()
  processDeleteQueue()
}

function isMailboxDeleteQueued(mailboxID) {
  return deleteQueue.value.some((item) => item.id === mailboxID)
}

function isMailboxDeleting(mailboxID) {
  return deletingMailboxIDs.value.includes(mailboxID)
}

function isMailboxDeleteBusy(mailboxID) {
  return deleteConfirmID.value === mailboxID || isMailboxDeleting(mailboxID) || isMailboxDeleteQueued(mailboxID)
}

function mailboxDeleteAccountKey(mailbox) {
  return mailbox.accountID || 'unknown-account'
}

function updateDeleteProgress() {
  const finished = deleteSucceeded + deleteFailed
  const running = deletingMailboxIDs.value.length
  const waiting = deleteQueue.value.length
  const total = finished + running + waiting
  if (!total) return
  deleteNoticeID = updateToast(
    deleteNoticeID,
    `删除邮箱：执行中 ${running}｜排队中 ${waiting}｜已完成 ${finished}/${total}（成功 ${deleteSucceeded}，失败 ${deleteFailed}）`,
    'info',
    0,
  )
}

function finishDeleteProgress() {
  const total = deleteSucceeded + deleteFailed
  const type = deleteFailed ? (deleteSucceeded ? 'warning' : 'error') : 'success'
  const text = deleteFailed
    ? `删除邮箱已结束：成功 ${deleteSucceeded}｜失败 ${deleteFailed}${deleteLastError ? `；最近错误：${deleteLastError}` : ''}`
    : `删除邮箱已完成：成功 ${total}｜失败 0`
  deleteNoticeID = updateToast(deleteNoticeID, text, type, 7000)
}

function processDeleteQueue() {
  if (!deleteQueueRunning) deleteQueueRunning = true
  while (activeDeleteAccounts.size < maxConcurrentDeleteAccounts) {
    const queueIndex = deleteQueue.value.findIndex((mailbox) => !activeDeleteAccounts.has(mailboxDeleteAccountKey(mailbox)))
    if (queueIndex < 0) break
    const [mailbox] = deleteQueue.value.splice(queueIndex, 1)
    const accountKey = mailboxDeleteAccountKey(mailbox)
    activeDeleteAccounts.add(accountKey)
    deletingMailboxIDs.value = [...deletingMailboxIDs.value, mailbox.id]
    void executeMailboxDeletion(mailbox, accountKey)
  }
  updateDeleteProgress()
  if (deleteQueue.value.length || activeDeleteAccounts.size) return
  deleteQueueRunning = false
  finishDeleteProgress()
}

async function executeMailboxDeletion(mailbox, accountKey) {
  try {
    const suffix = mailbox.localOnly ? '?local_only=1' : ''
    await api(`/api/mailboxes/${mailbox.id}${suffix}`, { method: 'DELETE' })
    deleteSucceeded += 1
    unselectMailbox(mailbox.id)
    if (selected.value?.id === mailbox.id) {
      selected.value = null
      messages.value = []
    }
  } catch (err) {
    deleteFailed += 1
    deleteLastError = `${mailbox.email}：${err.message}`
  } finally {
    deletingMailboxIDs.value = deletingMailboxIDs.value.filter((id) => id !== mailbox.id)
    activeDeleteAccounts.delete(accountKey)
    await load({ silent: true })
    processDeleteQueue()
  }
}

async function deleteMailbox(mailbox, localOnly) {
  if (!mailbox) return
  if (deleteConfirmID.value) {
    flash('请先完成当前删除确认', true)
    return
  }
  if (isMailboxDeleteBusy(mailbox.id)) {
    flash(`${mailbox.email} 已在删除队列中`, true)
    return
  }
  deleteConfirmID.value = mailbox.id
  const text = localOnly
    ? '将先清空本地邮件，再删除本地邮箱记录；Apple 服务器上的隐私邮箱会保留。继续吗？'
    : `将根据本地已同步邮件保存的远端标识，把 ${mailbox.email} 对应的 Apple 邮件移入废纸篓并清空该账号的整个废纸篓，再删除 Apple 隐私邮箱和本地记录。未同步到本地的历史邮件不会参与定位，此操作不可恢复，继续吗？`
  try {
    const confirmed = await confirmAction({
      title: localOnly ? '只删除本地记录' : '彻底删除隐私邮箱',
      message: text,
      confirmText: localOnly ? '确认删除本地记录' : '确认彻底删除',
      tone: 'danger',
    })
    if (!confirmed) return
    enqueueMailboxDeletions([{
      id: mailbox.id,
      email: mailbox.email,
      account_id: mailbox.account_id || mailbox.accountID || '',
      localOnly,
    }])
  } finally {
    deleteConfirmID.value = ''
  }
}

function removeMailbox(localOnly) {
  return deleteMailbox(selected.value, localOnly)
}

async function removeMailboxFromRow(mailbox, localOnly = false) {
  return deleteMailbox(mailbox, localOnly)
}

function goToPage(targetPage) {
  if (loading.value) return
  const nextPage = Math.max(1, Math.min(result.value.total_pages, targetPage))
  if (nextPage === page.value) return
  page.value = nextPage
  load()
}

function move(delta) {
  goToPage(page.value + delta)
}

function handlePageKeydown(event) {
  if (event.key !== 'Escape') return
  if (selectedMessage.value) selectedMessage.value = null
  else if (codeDialogOpen.value) closeCodeDialog()
  else if (quickEditOpen.value) closeQuickEdit()
  else if (showImport.value) showImport.value = false
  else if (showSync.value) showSync.value = false
  else if (showBulkDelete.value) closeBulkDeleteDialog()
  else if (selected.value) selected.value = null
}

watch(query, () => {
  clearTimeout(searchTimer)
  searchTimer = setTimeout(() => { page.value = 1; load() }, 300)
})
watch(accountID, () => { page.value = 1; load() })
watch(status, () => { page.value = 1; load() })
watch([selected, codeDialogOpen, quickEditOpen, showImport, showSync, showBulkDelete], ([mailbox, codeOpen, quickEditOpenValue, importOpen, syncOpen, bulkDeleteOpen]) => {
  document.body.style.overflow = mailbox || codeOpen || quickEditOpenValue || importOpen || syncOpen || bulkDeleteOpen ? 'hidden' : ''
})
watch(selected, (value) => {
  if (!value) selectedMessage.value = null
})
onMounted(() => {
  document.addEventListener('keydown', handlePageKeydown)
  Promise.all([load(), loadAccounts(), loadAppleMailCleanupStatus(), loadExistingMailboxMessageSyncStatus()])
  realtimeUnsubscribe = subscribeRealtime(['mailbox', 'mailbox-lease', 'message', 'apple-account', 'apple-mail-cleanup', 'mailbox-message-sync'], scheduleRealtimeMailboxRefresh)
  autoRefreshTimer = window.setInterval(refreshMailboxPool, 30000)
  mailboxLayoutObserver = new ResizeObserver(scheduleMailboxTableSize)
  if (mailboxCommandBar.value) mailboxLayoutObserver.observe(mailboxCommandBar.value)
  if (mailboxPagination.value) mailboxLayoutObserver.observe(mailboxPagination.value)
  const scrollContainer = mailboxTableViewport.value?.closest('.page-scroll')
  if (scrollContainer) mailboxLayoutObserver.observe(scrollContainer)
  window.addEventListener('resize', scheduleMailboxTableSize)
})
onBeforeUnmount(() => {
  clearTimeout(searchTimer)
  clearTimeout(loadingTimer)
  clearTimeout(codeBusyTimer)
  clearTimeout(tableResizeTimer)
  clearTimeout(realtimeRefreshTimer)
  clearTimeout(syncExistingMessagesIntroTimer)
  clearTimeout(syncExistingMessagesStatusTimer)
  pendingRealtimeResources.clear()
  window.clearInterval(autoRefreshTimer)
  realtimeUnsubscribe()
  window.removeEventListener('resize', scheduleMailboxTableSize)
  mailboxLayoutObserver?.disconnect()
  document.body.style.overflow = ''
  document.removeEventListener('keydown', handlePageKeydown)
})
</script>

<template>
  <div class="mailbox-page">
    <section class="panel mailbox-workbench">
      <div ref="mailboxCommandBar" class="mailbox-command-bar">
        <div class="mailbox-command-filters">
          <label class="field-wrap mailbox-search"><Search :size="15" class="field-icon" /><input v-model="query" class="field field-leading" type="search" placeholder="搜索邮箱、标签或备注" aria-label="搜索邮箱、标签或备注" /></label>
          <CardSelect v-model="accountID" class="mailbox-account-filter" :options="accountFilterOptions" aria-label="账号" compact />
          <CardSelect v-model="status" class="mailbox-status-filter" :options="mailboxStatusOptions" aria-label="邮箱状态" compact />
        </div>
        <div class="mailbox-command-actions">
          <button type="button" class="secondary-button mailbox-command-button" :disabled="isBusy('sync-existing') || isBusy('sync-existing-messages') || existingMailboxMessageSync.running" @click="openSyncDialog"><LoaderCircle v-if="isBusy('sync-existing')" :size="14" class="animate-spin" /><CloudDownload v-else :size="14" />{{ isBusy('sync-existing') ? '正在同步邮箱' : '同步已有邮箱' }}</button>
          <button type="button" class="secondary-button mailbox-command-button" :disabled="isBusy('sync-existing-messages') || isBusy('sync-existing') || existingMailboxMessageSync.running" :title="mailboxMessageSyncBulkHint" @click="syncExistingMailboxMessages"><LoaderCircle v-if="isBusy('sync-existing-messages') || existingMailboxMessageSync.running" :size="14" class="animate-spin" /><MailOpen v-else :size="14" />{{ existingMailboxMessageSync.running ? `正在同步 ${existingMailboxMessageSync.completed_accounts || 0}/${existingMailboxMessageSync.total_accounts || 0}` : isBusy('sync-existing-messages') ? '正在启动同步' : '同步已有邮箱邮件' }}</button>
          <button type="button" class="secondary-button mailbox-command-button" :disabled="isBusy('health-check') || isBusy('sync-existing') || isBusy('sync-existing-messages')" title="逐个登录检查池里的邮箱还能不能读信。判定为凭据失效的会被标成失败、移出邮箱池；只是连不上的会被标成「连不上」，只改显示、不动邮箱池" @click="runMailboxHealthCheck"><LoaderCircle v-if="isBusy('health-check')" :size="14" class="animate-spin" /><ShieldCheck v-else :size="14" />{{ isBusy('health-check') ? (healthCheck.total ? `正在体检 ${healthCheck.done}/${healthCheck.total}` : '正在体检') : '体检邮箱能否读取' }}</button>
          <button type="button" class="secondary-button mailbox-command-button" :disabled="isBusy('import')" @click="openImportDialog"><LoaderCircle v-if="isBusy('import')" :size="14" class="animate-spin" /><MailPlus v-else :size="14" />{{ isBusy('import') ? '正在导入邮箱' : '导入本地邮箱' }}</button>
          <button type="button" class="secondary-button mailbox-command-button" :disabled="isBusy('external-import')" title="导入没有母号、没有密码，只有取件接口的邮箱" @click="openExternalImportDialog"><LoaderCircle v-if="isBusy('external-import')" :size="14" class="animate-spin" /><CloudDownload v-else :size="14" />导入外部取码邮箱</button>
          <button type="button" class="secondary-button mailbox-command-button" :disabled="isBusy('clean-summary') || isBusy('clean-start') || appleMailCleanup.running" title="扫描并彻底删除全部 Apple 账号的云端和本地邮件" @click="cleanAllAppleMail"><LoaderCircle v-if="isBusy('clean-summary') || isBusy('clean-start') || appleMailCleanup.running" :size="14" class="animate-spin" /><CloudOff v-else :size="14" />{{ isBusy('clean-summary') ? '正在统计邮件' : isBusy('clean-start') ? '正在启动清理' : appleMailCleanup.running ? `正在清理 ${appleMailCleanup.completed || 0}/${appleMailCleanup.total_accounts || 0}` : '全部彻底清理 Apple 邮件' }}</button>
          <button type="button" class="secondary-button mailbox-command-button mailbox-command-button-danger" :disabled="isBusy('bulk-delete-resolve')" title="按邮箱地址批量彻底删除 Apple 云端和本地邮箱" @click="openBulkDeleteDialog"><Trash2 :size="14" />批量删除指定邮箱</button>
          <button type="button" class="secondary-button mailbox-command-button mailbox-command-button-danger" :disabled="!selectedDeletableCount || deleteConfirmID === 'selected'" :title="selectedDeletableCount ? `删除选中的 ${selectedDeletableCount} 个邮箱（彻底删除 ${selectedDeletePlan.remote.length} 个，只删本地 ${selectedDeletePlan.local.length} 个${selectedDeletePlan.blocked.length ? `，跳过 ${selectedDeletePlan.blocked.length} 个主地址` : ''}）` : '请先选择未进入删除队列的邮箱'" @click="removeSelectedMailboxes"><LoaderCircle v-if="deleteConfirmID === 'selected'" :size="14" class="animate-spin" /><Trash2 v-else :size="14" />删除选中{{ selectedDeletableCount ? `（${selectedDeletableCount}）` : '' }}</button>
        </div>
      </div>
      <div v-if="loadingVisible" class="mailbox-loading-mask"><div><LoaderCircle :size="16" class="animate-spin" />正在加载邮箱</div></div>
      <div ref="mailboxTableViewport" class="mailbox-table-viewport" :style="{ '--mailbox-table-height': `${mailboxTableHeight}px`, '--mailbox-empty-height': `${mailboxEmptyHeight}px` }">
        <table class="mailbox-pool-table" :class="{ 'mailbox-pool-table-empty': !result.items?.length }">
          <colgroup v-if="result.items?.length"><col class="mailbox-col-id" /><col class="mailbox-col-account" /><col class="mailbox-col-email" /><col class="mailbox-col-label" /><col class="mailbox-col-status" /><col class="mailbox-col-channel" /><col class="mailbox-col-count" /><col class="mailbox-col-sync" /><col class="mailbox-col-actions" /></colgroup>
          <thead><tr><th><label class="mailbox-select-all"><input type="checkbox" :checked="allPageMailboxesSelected" :indeterminate="somePageMailboxesSelected" :disabled="!result.items?.length" aria-label="全选当前页邮箱" @change="toggleAllMailboxSelection" /><span>ID</span></label></th><th>账号</th><th>邮箱</th><th>标签 / 备注</th><th>状态</th><th>API / iCloud</th><th>收件</th><th>最近同步</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="mailbox in result.items" :key="mailbox.id" :class="{ 'mailbox-row-selected': isMailboxSelected(mailbox.id) }">
              <td class="mailbox-id-cell"><label class="mailbox-id-wrap"><input type="checkbox" :checked="isMailboxSelected(mailbox.id)" :disabled="isMailboxDeleteBusy(mailbox.id)" :aria-label="`选择邮箱 ${mailbox.email}`" @change="toggleMailboxSelection(mailbox)" /><span :title="mailbox.id">{{ mailbox.id }}</span></label></td>
              <td class="mailbox-account-cell"><span :title="mailboxAppleAccount(mailbox)">{{ mailboxAppleAccount(mailbox) }}</span><span class="ml-1 shrink-0 rounded bg-slate-100 px-1.5 py-0.5 text-[10px] font-medium text-slate-500 dark:bg-slate-700 dark:text-slate-300" :title="`服务商：${providerLabel(mailboxProvider(mailbox))}`">{{ providerLabel(mailboxProvider(mailbox)) }}</span></td>
              <td class="mailbox-email-cell"><button type="button" class="mailbox-email-copy" :title="`点击复制邮箱：${mailbox.email}`" @click.stop="copyMailboxEmail(mailbox)">{{ mailbox.email }}</button></td>
              <td class="mailbox-label-cell"><span :title="mailbox.label || '—'">{{ mailbox.label || '—' }}</span><button type="button" :title="mailbox.note ? `点击修改备注：${mailbox.note}` : '点击添加备注'" @click.stop="openQuickEdit(mailbox, 'note')">{{ mailbox.note || '添加备注' }}</button></td>
              <td><button type="button" :class="statusClass(mailbox.status)" class="mailbox-status-button" title="点击修改邮箱状态" @click.stop="openQuickEdit(mailbox, 'status')">{{ statusLabel(mailbox.status) }}</button></td>
              <td><div class="mailbox-channel-badges"><span :class="mailbox.api_active ? 'text-emerald-600 bg-emerald-50 dark:bg-emerald-950/40' : 'text-slate-400 bg-slate-100 dark:bg-slate-700'">API</span><span :class="mailbox.icloud_active ? 'text-sky-600 bg-sky-50 dark:bg-sky-950/40' : 'text-slate-400 bg-slate-100 dark:bg-slate-700'">iCloud</span></div></td>
              <td class="mailbox-count">{{ mailbox.receive_count || 0 }}</td>
              <td class="mailbox-sync-time">{{ formatTime(mailbox.last_sync_at) }}</td>
              <td>
                <div class="mailbox-row-actions">
                  <button class="mailbox-action-button mailbox-action-sync" :class="{ 'mailbox-action-sync-selected': rowBusyAction(mailbox.id) === 'sync' }" :disabled="Boolean(rowBusyAction(mailbox.id)) || isMailboxDeleteBusy(mailbox.id)" :title="mailboxMessageSyncHint" @click.stop="quickSyncMailbox(mailbox)"><LoaderCircle v-if="rowBusyAction(mailbox.id) === 'sync'" :size="12" class="animate-spin" /><RefreshCw v-else :size="12" />同步</button>
                  <button class="mailbox-action-button mailbox-action-code" :class="{ 'mailbox-action-code-selected': rowBusyAction(mailbox.id) === 'code' || (codeDialogOpen && codeMailbox?.id === mailbox.id) }" :disabled="Boolean(rowBusyAction(mailbox.id)) || isMailboxDeleteBusy(mailbox.id)" title="获取该邮箱的最新验证码" @click.stop="quickGetCode(mailbox)"><LoaderCircle v-if="codeBusyVisible === `code-row:${mailbox.id}`" :size="12" class="animate-spin" /><KeyRound v-else :size="12" />取码</button>
                  <button class="mailbox-action-button mailbox-action-detail" :class="{ 'mailbox-action-detail-selected': selected?.id === mailbox.id }" :disabled="Boolean(rowBusyAction(mailbox.id)) || isMailboxDeleteBusy(mailbox.id)" title="查看邮箱详情" @click.stop="openMailbox(mailbox)"><LoaderCircle v-if="rowBusyAction(mailbox.id) === 'detail'" :size="12" class="animate-spin" /><MailOpen v-else :size="12" />详情</button>
                  <span v-if="isPrimaryMailbox(mailbox)" class="inline-flex items-center px-1.5 py-1 text-[10px] font-bold text-slate-400 dark:text-slate-500" title="主地址就是这一行账号本身。删掉这条池子记录后，寄到主地址的信会因为匹配不到任何本地地址而被丢弃，所以这里不提供删除。">主地址</span>
                  <button v-else-if="supportsRemoteDelete(mailbox)" class="mailbox-action-button mailbox-action-delete" :class="{ 'mailbox-action-delete-selected': isMailboxDeleteBusy(mailbox.id) }" :disabled="Boolean(rowBusyAction(mailbox.id)) || isMailboxDeleteBusy(mailbox.id)" :title="isMailboxDeleting(mailbox.id) ? '正在清理已同步邮件并删除隐私邮箱' : isMailboxDeleteQueued(mailbox.id) ? '已加入彻底删除队列' : '清理已同步的远端邮件后彻底删除隐私邮箱'" @click.stop="removeMailboxFromRow(mailbox)"><LoaderCircle v-if="isMailboxDeleting(mailbox.id)" :size="12" class="animate-spin" /><LoaderCircle v-else-if="isMailboxDeleteQueued(mailbox.id)" :size="12" class="animate-spin" /><Trash2 v-else :size="12" />{{ isMailboxDeleting(mailbox.id) ? '删除中' : isMailboxDeleteQueued(mailbox.id) ? '排队中' : '删除' }}</button>
                  <button v-else class="mailbox-action-button mailbox-action-delete" :class="{ 'mailbox-action-delete-selected': isMailboxDeleteBusy(mailbox.id) }" :disabled="Boolean(rowBusyAction(mailbox.id)) || isMailboxDeleteBusy(mailbox.id)" :title="isMailboxDeleting(mailbox.id) ? '正在删除本地记录' : isMailboxDeleteQueued(mailbox.id) ? '已加入删除队列' : '只删除本地记录和已同步邮件。这类邮箱没有 Apple 远端可清理，所以不做彻底删除。'" @click.stop="removeMailboxFromRow(mailbox, true)"><LoaderCircle v-if="isMailboxDeleting(mailbox.id)" :size="12" class="animate-spin" /><LoaderCircle v-else-if="isMailboxDeleteQueued(mailbox.id)" :size="12" class="animate-spin" /><Trash2 v-else :size="12" />{{ isMailboxDeleting(mailbox.id) ? '删除中' : isMailboxDeleteQueued(mailbox.id) ? '排队中' : '只删本地' }}</button>
                </div>
              </td>
            </tr>
            <tr v-if="!loading && !result.items?.length" class="mailbox-empty-row"><td colspan="9" class="mailbox-empty-cell"><span><Boxes :size="20" /></span><strong>没有符合条件的邮箱</strong><small>从 iCloud、Microsoft 或 Google 账号页创建或同步邮箱后会显示在这里。</small></td></tr>
          </tbody>
        </table>
      </div>
      <div ref="mailboxPagination" class="mailbox-pagination"><span>第 {{ result.page }} / {{ result.total_pages }} 页　总 {{ result.total }} 个邮箱</span><div><button type="button" class="secondary-button" :disabled="loading || result.page <= 1" title="跳转到首页" aria-label="跳转到首页" @click="goToPage(1)"><ChevronsLeft :size="15" /></button><button type="button" class="secondary-button" :disabled="loading || result.page <= 1" title="上一页" aria-label="上一页" @click="move(-1)"><ChevronLeft :size="15" /></button><button type="button" class="secondary-button" :disabled="loading || result.page >= result.total_pages" title="下一页" aria-label="下一页" @click="move(1)"><ChevronRight :size="15" /></button><button type="button" class="secondary-button" :disabled="loading || result.page >= result.total_pages" title="跳转到末页" aria-label="跳转到末页" @click="goToPage(result.total_pages)"><ChevronsRight :size="15" /></button></div></div>
    </section>

    <Teleport to="body">
      <div v-if="healthCheckOpen" class="mailbox-dialog-backdrop" role="presentation" @click.self="!healthCheck.running && closeHealthCheck()">
        <section data-health-panel class="panel mailbox-operation-dialog mailbox-health-dialog" role="dialog" aria-modal="true" aria-labelledby="mailbox-health-check-title">
          <header class="mailbox-dialog-heading">
            <div>
              <h2 id="mailbox-health-check-title"><ShieldCheck :size="18" />体检邮箱能否读取</h2>
              <p>逐个登录池里的邮箱读一次，不发信、不取正文。结论会写回邮箱状态。</p>
            </div>
            <button type="button" class="icon-button" title="关闭" :disabled="healthCheck.running" @click="closeHealthCheck"><X :size="16" /></button>
          </header>
          <div class="mailbox-health-summary">
            <template v-if="healthCheck.running">
              <span class="font-medium">正在体检 {{ healthCheck.done }}/{{ healthCheck.total || '…' }}</span>
              <span class="text-slate-500 dark:text-slate-400">判定为凭据失效的会被标成失败并移出邮箱池</span>
            </template>
            <template v-else-if="healthCheck.report">
              <span class="font-medium">体检完成</span>
              <span class="text-emerald-600 dark:text-emerald-400">能读 {{ healthCheck.report.readable }}</span>
              <span :class="healthCheck.report.unreadable ? 'text-rose-600 dark:text-rose-400' : 'text-slate-500 dark:text-slate-400'">读不了 {{ healthCheck.report.unreadable }}</span>
              <span v-if="healthCheck.report.never_checked" class="text-amber-600 dark:text-amber-400" title="从来没有被检查过的邮箱，收信数为 0 只代表不知道">从未检查过 {{ healthCheck.report.never_checked }}</span>
              <span v-if="healthCheck.report.missing" class="text-slate-500 dark:text-slate-400">本地已无记录 {{ healthCheck.report.missing }}</span>
              <span v-if="healthCheck.report.not_covered" class="text-slate-500 dark:text-slate-400" title="这些服务商走各自的授权接口读信，本次体检没有检查它们">本次未覆盖 {{ healthCheck.report.not_covered }}</span>
              <span v-if="healthCheck.report.demoted" class="font-medium text-rose-600 dark:text-rose-400">已从池里摘掉 {{ healthCheck.report.demoted }} 个</span>
              <span v-if="healthCheck.report.unreadable > healthCheck.report.demoted" class="text-amber-600 dark:text-amber-400" title="这些读不了但没有摘：网络类失败不足以判定凭据失效，或者它的状态是被手工设过的">未摘 {{ healthCheck.report.unreadable - healthCheck.report.demoted }} 个</span>
            </template>
          </div>
          <ul v-if="!healthCheck.running && healthCheck.report?.unreadable" class="mailbox-health-list">
            <li v-for="item in healthCheck.report.items.filter((row) => row.covered && !row.readable)" :key="item.id">
              <span class="font-mono">{{ item.email }}</span>
              <span class="text-slate-500 dark:text-slate-400">{{ item.reason || item.code }}</span>
              <span v-if="item.demoted" class="rounded bg-rose-100 px-1.5 py-0.5 font-medium text-rose-700 dark:bg-rose-950/50 dark:text-rose-300">已从池里摘掉</span>
              <span v-else-if="item.account_status" class="rounded bg-amber-100 px-1.5 py-0.5 font-medium text-amber-700 dark:bg-amber-950/50 dark:text-amber-300">账号已标为{{ accountStatusLabel(item) }}，未摘出邮箱池</span>
              <span v-else-if="item.offered_while_unreadable" class="rounded bg-amber-100 px-1.5 py-0.5 font-medium text-amber-700 dark:bg-amber-950/50 dark:text-amber-300">没能摘掉</span>
              <span v-else class="rounded bg-slate-200 px-1.5 py-0.5 text-slate-600 dark:bg-slate-700 dark:text-slate-300">状态未改动（{{ item.status }}）</span>
              <span v-if="item.never_checked" class="rounded bg-amber-100 px-1.5 py-0.5 text-amber-700 dark:bg-amber-950/50 dark:text-amber-300">从未检查过</span>
            </li>
          </ul>
          <p v-else-if="!healthCheck.running && healthCheck.report" class="mailbox-health-empty">池里的邮箱都能读到信。</p>
          <footer class="mailbox-dialog-actions">
            <button type="button" class="secondary-button" :disabled="healthCheck.running" @click="closeHealthCheck">关闭</button>
          </footer>
        </section>
      </div>

      <div v-if="showImport" class="mailbox-dialog-backdrop" role="presentation" @click.self="showImport = false">
        <form class="panel mailbox-operation-dialog mailbox-import-dialog" role="dialog" aria-modal="true" aria-labelledby="import-mailbox-title" @submit.prevent="importMailbox">
          <header class="mailbox-dialog-heading"><div><h2 id="import-mailbox-title"><MailPlus :size="18" />导入已有隐私邮箱</h2><p>只创建或更新本地记录，不会在 Apple 服务器新建邮箱。</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('import')" @click="showImport = false"><X :size="16" /></button></header>
          <div class="mailbox-dialog-grid">
            <div class="form-group"><span class="form-label">绑定账号</span><CardSelect v-model="mailboxImport.account_id" :options="accountOptions" placeholder="请选择账号" aria-label="绑定账号" /></div>
            <label class="form-group"><span class="form-label">隐私邮箱地址</span><input v-model.trim="mailboxImport.email" type="email" class="field" placeholder="example@icloud.com" required /></label>
            <label class="form-group"><span class="form-label">标签</span><input v-model.trim="mailboxImport.label" class="field" placeholder="例如：手动导入" /></label>
            <label class="form-group"><span class="form-label">备注</span><input v-model.trim="mailboxImport.note" class="field" placeholder="可选" /></label>
          </div>
          <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('import')" @click="showImport = false">取消</button><button class="primary-button" :disabled="isBusy('import')"><LoaderCircle v-if="isBusy('import')" :size="15" class="animate-spin" /><MailPlus v-else :size="15" />保存本地邮箱</button></footer>
        </form>
      </div>

      <div v-if="showExternalImport" class="mailbox-dialog-backdrop" role="presentation" @click.self="showExternalImport = false">
        <form class="panel mailbox-operation-dialog mailbox-import-dialog" role="dialog" aria-modal="true" aria-labelledby="external-import-title" @submit.prevent="importExternalMailboxes">
          <header class="mailbox-dialog-heading"><div><h2 id="external-import-title"><CloudDownload :size="18" />导入外部取码邮箱</h2><p>适用于没有母号、也没有登录密码，只有一个取件接口的邮箱。</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('external-import')" @click="showExternalImport = false"><X :size="16" /></button></header>
          <div class="mailbox-dialog-grid">
            <div class="form-group mailbox-dialog-wide">
              <span class="form-label">方式一：粘贴内容</span>
              <textarea v-model="externalImport.raw" class="field font-mono" rows="6" placeholder="tippet_crooked_7j@icloud.com https://ic-mail.example.com/api/v1/access/TOKEN/mailboxes/tippet_crooked_7j@icloud.com/code"></textarea>
            </div>
            <div class="form-group mailbox-dialog-wide">
              <span class="form-label">方式二：选择奖品 JSON 文件（可多选）</span>
              <div class="apple-inline-field">
                <button type="button" class="secondary-button" @click="externalFileInput?.click()"><FileUp :size="14" />选择文件</button>
                <input ref="externalFileInput" type="file" accept=".json,application/json" multiple class="mailbox-hidden-file" @change="pickExternalFiles" />
                <span class="form-help">读到的内容会填进上面的框，两种方式可以混用。</span>
              </div>
            </div>
            <label class="form-group"><span class="form-label">标签</span><input v-model.trim="externalImport.label" class="field" placeholder="留空则用邮箱地址" /></label>
            <label class="form-group"><span class="form-label">备注</span><input v-model.trim="externalImport.note" class="field" placeholder="可选" /></label>
          </div>
          <p class="apple-dialog-intro">已解析出 <strong>{{ externalPreviewCount }}</strong> 条。两种方式都认：直接粘贴的文本（含 Markdown 表格）和奖品 JSON（<code>{"value":"邮箱----取码地址"}</code>，单个或数组都行）。取码地址里含访问令牌，保存时会加密。</p>
          <p class="apple-dialog-intro">这类邮箱取信靠的是那个接口令牌，不是 Apple 登录态：导入时会先探活一次并如实报告结果，令牌失效会直接说明，不会伪装成「还没收到验证码」。导入后在邮箱池里点「同步」即可取码。</p>
          <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('external-import')" @click="showExternalImport = false">取消</button><button class="primary-button" :disabled="isBusy('external-import') || !externalPreviewCount"><LoaderCircle v-if="isBusy('external-import')" :size="15" class="animate-spin" /><CloudDownload v-else :size="15" />导入 {{ externalPreviewCount }} 个邮箱</button></footer>
        </form>
      </div>

      <div v-if="showSync" class="mailbox-dialog-backdrop" role="presentation" @click.self="showSync = false">
        <form class="panel mailbox-operation-dialog mailbox-sync-dialog" role="dialog" aria-modal="true" aria-labelledby="sync-mailboxes-title" @submit.prevent="syncExistingMailboxes">
          <header class="mailbox-dialog-heading"><div><h2 id="sync-mailboxes-title"><CloudDownload :size="18" />同步已有邮箱</h2><p>从所选账号读取已有邮箱地址，并更新到本地邮箱池。iCloud 走 Hide My Email，Microsoft 和 Google 走别名导入。</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('sync-existing')" @click="showSync = false"><X :size="16" /></button></header>
          <div class="mailbox-sync-fields"><div class="form-group"><span class="form-label">同步范围</span><CardSelect v-model="syncAccountID" :options="syncAccountOptions" aria-label="同步范围" /><span class="form-help">同步使用账号已保存的 iCloud Web 旧接口登录态。</span></div></div>
          <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('sync-existing')" @click="showSync = false">取消</button><button class="primary-button" :disabled="isBusy('sync-existing')"><LoaderCircle v-if="isBusy('sync-existing')" :size="15" class="animate-spin" /><CloudDownload v-else :size="15" />{{ isBusy('sync-existing') ? '同步中' : '开始同步' }}</button></footer>
        </form>
      </div>

      <div v-if="showBulkDelete" class="mailbox-dialog-backdrop" role="presentation" @click.self="closeBulkDeleteDialog">
        <form class="panel mailbox-operation-dialog mailbox-bulk-delete-dialog" role="dialog" aria-modal="true" aria-labelledby="bulk-delete-mailboxes-title" @submit.prevent="submitBulkDeleteEmails">
          <header class="mailbox-dialog-heading"><div><h2 id="bulk-delete-mailboxes-title"><Trash2 :size="18" />批量删除指定邮箱</h2><p>一行输入一个邮箱。提交后会根据本地已同步邮件的远端标识逐个清理 Apple 邮件、清空所属账号的整个废纸篓，再删除 Apple 隐私邮箱、本地邮件和邮箱记录。</p></div><button type="button" class="icon-button" title="关闭" :disabled="isBusy('bulk-delete-resolve')" @click="closeBulkDeleteDialog"><X :size="16" /></button></header>
          <div class="mailbox-bulk-delete-fields">
            <label class="form-group"><span class="form-label">邮箱地址列表</span><textarea v-model="bulkDeleteEmails" class="field mailbox-bulk-delete-input" placeholder="example1@icloud.com&#10;example2@icloud.com" spellcheck="false" autofocus required /><span class="form-help">已识别 {{ bulkDeleteEmailCount }} 个邮箱；重复地址会自动合并。</span></label>
            <div v-if="bulkDeleteError" class="mailbox-bulk-delete-error" role="alert">{{ bulkDeleteError }}</div>
          </div>
          <footer class="mailbox-dialog-actions"><button type="button" class="secondary-button" :disabled="isBusy('bulk-delete-resolve')" @click="closeBulkDeleteDialog">取消</button><button class="primary-button mailbox-bulk-delete-submit" :disabled="isBusy('bulk-delete-resolve') || !bulkDeleteEmailCount"><LoaderCircle v-if="isBusy('bulk-delete-resolve')" :size="15" class="animate-spin" /><Trash2 v-else :size="15" />{{ isBusy('bulk-delete-resolve') ? '正在读取邮箱' : `开始彻底删除（${bulkDeleteEmailCount}）` }}</button></footer>
        </form>
      </div>
    </Teleport>

    <FormDialog class="mailbox-quick-edit-dialog" :open="quickEditOpen" :title="quickEditTitle" :description="quickEditMailbox?.email || ''" :busy="isBusy('quick-edit')" @close="closeQuickEdit" @submit="saveQuickEdit">
      <label v-if="quickEditField === 'note'" class="form-group"><span class="form-label">备注</span><textarea v-model="quickEdit.note" class="field min-h-24 resize-none" maxlength="1000" placeholder="请输入邮箱备注，留空可清除备注" autofocus /></label>
      <div v-else class="form-group"><span class="form-label">邮箱状态</span><CardSelect v-model="quickEdit.status" :options="mailboxDetailStatusOptions" aria-label="邮箱状态" /></div>
    </FormDialog>

    <div v-if="selected" class="fixed inset-0 z-40 !m-0 flex items-center justify-center bg-slate-950/55 p-3 backdrop-blur-[3px] sm:p-5" role="presentation" @click.stop>
      <aside class="mailbox-detail-dialog max-h-[calc(100vh-1.5rem)] w-full max-w-2xl overflow-y-auto rounded-2xl border border-slate-200 bg-slate-50 shadow-2xl dark:border-slate-700 dark:bg-slate-950 sm:max-h-[calc(100vh-2.5rem)]" role="dialog" aria-modal="true" aria-labelledby="mailbox-detail-title">
        <header class="sticky top-0 z-10 rounded-t-2xl border-b border-slate-200 bg-white/95 px-4 py-3 backdrop-blur dark:border-slate-800 dark:bg-slate-900/95">
          <div class="flex items-start justify-between gap-3">
            <div class="min-w-0">
              <div class="mb-1.5 flex flex-wrap items-center gap-1.5">
                <span :class="statusClass(selected.status)" class="rounded-md px-2 py-0.5 text-[10px] font-bold">{{ statusLabel(selected.status) }}</span>
                <span :class="selected.api_active ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/60 dark:text-emerald-200' : 'bg-slate-200 text-slate-500 dark:bg-slate-700 dark:text-slate-300'" class="rounded-md px-2 py-0.5 text-[10px] font-bold">API</span>
                <span :class="selected.icloud_active ? 'bg-sky-100 text-sky-700 dark:bg-sky-900/60 dark:text-sky-200' : 'bg-slate-200 text-slate-500 dark:bg-slate-700 dark:text-slate-300'" class="rounded-md px-2 py-0.5 text-[10px] font-bold">iCloud</span>
              </div>
              <h2 id="mailbox-detail-title" class="truncate text-base font-black">{{ selected.email }}</h2>
              <p v-if="selected.forward_to_email" class="mt-0.5 truncate text-[10px] text-sky-500">转发主号：{{ selected.forward_to_email }}</p>
              <p class="mt-0.5 truncate font-mono text-[10px] text-slate-400">{{ selected.id }}</p>
              <p v-if="selected.active_lease_id" class="mt-0.5 truncate font-mono text-[10px] text-violet-500">当前租约：{{ selected.active_lease_id }}</p>
            </div>
            <button class="icon-button h-8 w-8 rounded-lg" title="关闭" @click="selected = null"><X :size="17" /></button>
          </div>
        </header>

        <div class="space-y-3 p-4">
          <div class="grid grid-cols-2 gap-2">
            <button class="detail-button detail-button-primary" :disabled="isBusy('sync') || isMailboxDeleteBusy(selected.id)" :title="mailboxMessageSyncHint" @click="syncMailbox"><RefreshCw :size="14" :class="isBusy('sync') ? 'animate-spin' : ''" />同步邮件</button>
            <button class="detail-button detail-button-secondary" :disabled="isBusy('code') || isMailboxDeleteBusy(selected.id)" @click="getCode"><LoaderCircle v-if="codeBusyVisible === 'code'" :size="14" class="animate-spin" /><KeyRound v-else :size="14" />获取验证码</button>
            <button v-if="selected.provider !== 'external'" class="detail-button detail-button-secondary col-span-2" :disabled="isBusy('backfill') || isMailboxDeleteBusy(selected.id)" :title="mailboxBackfillHint" @click="backfillMailbox"><LoaderCircle v-if="isBusy('backfill')" :size="14" class="animate-spin" /><CloudDownload v-else :size="14" />{{ isBusy('backfill') ? '正在回填历史邮件' : '回填历史邮件' }}</button>
          </div>

          <form class="rounded-xl border border-slate-200 bg-white p-3 dark:border-slate-800 dark:bg-slate-900" @submit.prevent="saveStatus">
            <div class="mb-1 flex flex-wrap items-center justify-between gap-2">
              <h3 class="text-xs font-black text-slate-700 dark:text-slate-200">状态与接收</h3>
              <div class="ml-auto flex items-center gap-2"><div class="flex items-center gap-1.5"><span class="whitespace-nowrap text-[10px] font-bold text-slate-400">使用状态</span><CardSelect v-model="edit.status" class="detail-status-select" :options="mailboxDetailStatusOptions" aria-label="使用状态" compact /></div><button class="detail-button detail-button-secondary h-8" :disabled="isBusy('save') || isMailboxDeleteBusy(selected.id)"><LoaderCircle v-if="isBusy('save')" :size="13" class="animate-spin" /><Save v-else :size="13" />保存</button></div>
            </div>
            <div>
              <label class="block space-y-1.5"><span class="text-[11px] font-bold text-slate-500 dark:text-slate-300">备注</span><input v-model="edit.note" type="text" class="field detail-field" maxlength="1000" placeholder="可选备注" /></label>
            </div>
            <div class="detail-setting-grid mt-3">
              <label class="detail-setting-row"><span><strong>公共取码 API</strong><small>控制外部接口取码</small></span><input v-model="edit.api_active" class="detail-switch" type="checkbox" /></label>
              <label class="detail-setting-row"><span><strong>iCloud 远端状态</strong><small>标记邮箱是否可收信</small></span><input v-model="edit.icloud_active" class="detail-switch" type="checkbox" /></label>
            </div>
          </form>

          <section class="rounded-xl border border-slate-200 bg-white p-3 dark:border-slate-800 dark:bg-slate-900">
            <div class="mb-2.5 flex items-start justify-between gap-3"><h3 class="text-xs font-black text-slate-700 dark:text-slate-200">本地邮件 <span class="ml-1 text-slate-400">{{ messages.length }}</span></h3><span class="whitespace-nowrap text-[10px] text-slate-400">同步于 {{ formatTime(selected.last_sync_at) }}</span></div>
            <div v-if="!messages.length" class="mail-message-list flex items-center justify-center text-xs text-slate-400">暂无本地邮件</div>
            <div v-else class="mail-message-list">
              <button v-for="item in messages" :key="item.id" type="button" class="mail-message-row" :title="`查看完整邮件：${item.subject || '无主题'}`" @click="openMessage(item)">
                <span class="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-sky-100 text-sky-600 dark:bg-sky-950/50 dark:text-sky-300"><MailOpen :size="15" /></span>
                <span class="min-w-0 flex-1 text-left"><strong class="block truncate text-xs">{{ item.subject || '无主题' }}</strong><small class="mt-0.5 block truncate text-[10px] text-slate-400">{{ item.from || '未知发件人' }}</small></span>
                <span class="flex shrink-0 items-center gap-1.5"><time class="text-[9px] text-slate-400">{{ formatMessageTime(item.received_at) }}</time><ChevronRight :size="14" class="text-slate-300 dark:text-slate-600" /></span>
              </button>
            </div>
          </section>

          <section class="rounded-xl border border-rose-200/70 bg-white p-3 dark:border-rose-950 dark:bg-slate-900">
            <div class="mb-2.5"><h3 class="text-xs font-black text-slate-700 dark:text-slate-200">清理与删除</h3><p class="mt-0.5 text-[10px] leading-4 text-slate-400">{{ deleteSectionNote(selected) }}</p></div>
            <div class="detail-setting-grid"><label class="detail-setting-row"><span><strong>移动已同步邮件</strong><small>移入 Apple 废纸篓</small></span><input v-model="remoteClean.move_synced" class="detail-switch" type="checkbox" /></label><label class="detail-setting-row"><span><strong>清空整个废纸篓</strong><small>彻底清除该 Apple 账号的废纸篓邮件</small></span><input v-model="remoteClean.empty_trash" class="detail-switch" type="checkbox" /></label></div>
            <div class="mt-2 grid gap-2 sm:grid-cols-2">
              <button v-if="supportsRemoteDelete(selected)" class="detail-button detail-button-secondary sm:col-span-2" :disabled="isBusy('clean') || isMailboxDeleteBusy(selected.id) || (!remoteClean.move_synced && !remoteClean.empty_trash)" @click="cleanRemote"><LoaderCircle v-if="isBusy('clean')" :size="14" class="animate-spin" /><CloudOff v-else :size="14" />清理 Apple 远端邮件</button>
              <button v-if="supportsRemoteDelete(selected)" class="detail-button detail-button-danger" :disabled="isMailboxDeleteBusy(selected.id)" @click="removeMailbox(false)"><LoaderCircle v-if="isMailboxDeleteBusy(selected.id)" :size="14" class="animate-spin" /><Trash2 v-else :size="14" />{{ isMailboxDeleting(selected.id) ? '删除中' : isMailboxDeleteQueued(selected.id) ? '排队中' : '彻底删除' }}</button>
              <button v-if="!isPrimaryMailbox(selected)" class="detail-button detail-button-ghost" :class="{ 'sm:col-span-2': !supportsRemoteDelete(selected) }" :disabled="isMailboxDeleteBusy(selected.id)" @click="removeMailbox(true)"><LoaderCircle v-if="isMailboxDeleteBusy(selected.id)" :size="14" class="animate-spin" /><ShieldX v-else :size="14" />{{ isMailboxDeleting(selected.id) ? '删除中' : isMailboxDeleteQueued(selected.id) ? '排队中' : '只删本地' }}</button>
              <p v-else class="sm:col-span-2 text-[10px] leading-4 text-slate-400">主地址就是这一行账号本身。删掉这条池子记录后，寄到主地址的信会因为匹配不到任何本地地址而被丢弃，所以这里不提供删除。</p>
            </div>
          </section>
        </div>
      </aside>
    </div>

    <div v-if="codeDialogOpen" class="fixed inset-0 z-[70] !m-0 flex items-center justify-center bg-slate-950/65 p-4 backdrop-blur-[4px]" role="presentation" @click.stop>
      <section class="panel mailbox-code-dialog overflow-hidden p-0 shadow-2xl" role="dialog" aria-modal="true" aria-labelledby="mailbox-code-title">
        <header class="mailbox-code-heading flex items-start justify-between gap-4 border-b border-slate-200 px-5 py-4 dark:border-slate-700">
          <div class="min-w-0">
            <div class="mb-1.5 flex items-center gap-1.5"><span class="rounded-md bg-emerald-100 px-2 py-0.5 text-[10px] font-bold text-emerald-700 dark:bg-emerald-950/60 dark:text-emerald-200">邮箱取码</span><span v-if="codeMailbox?.label" class="max-w-40 truncate rounded-md bg-slate-100 px-2 py-0.5 text-[10px] font-bold text-slate-500 dark:bg-slate-800 dark:text-slate-300">{{ codeMailbox.label }}</span></div>
            <h2 id="mailbox-code-title" class="truncate text-base font-black">{{ codeMailbox?.email || code?.email || '获取验证码' }}</h2>
            <p class="mt-1 text-[11px] text-slate-400">同步最新邮件并提取验证码</p>
          </div>
          <button class="icon-button h-8 w-8 rounded-lg" title="关闭验证码弹窗" :disabled="codeDialogBusy" @click="closeCodeDialog"><X :size="17" /></button>
        </header>

        <div class="mailbox-code-body p-5">
          <div v-if="codeDialogBusy" class="mailbox-code-state flex min-h-44 flex-col items-center justify-center gap-3 text-center">
            <span class="flex h-12 w-12 items-center justify-center rounded-2xl bg-emerald-50 text-emerald-600 dark:bg-emerald-950/50 dark:text-emerald-300"><LoaderCircle v-if="codeBusyVisible" :size="24" class="animate-spin" /><KeyRound v-else :size="23" /></span>
            <div><strong class="block text-sm">正在获取验证码</strong><span class="mt-1 block text-xs text-slate-400">正在同步并检查最新邮件，请稍候……</span></div>
          </div>

          <div v-else-if="code" class="mailbox-code-result space-y-4">
            <div class="mailbox-code-card rounded-2xl border border-emerald-200 bg-emerald-50 p-4 text-center dark:border-emerald-900 dark:bg-emerald-950/35">
              <div class="text-[11px] font-bold text-emerald-600 dark:text-emerald-300">最新验证码</div>
              <button type="button" class="mailbox-code-value" title="点击复制验证码" :aria-label="`复制验证码 ${code.code}`" @click="copyCode">{{ code.code }}</button>
              <div class="mt-2 truncate text-xs text-emerald-700/70 dark:text-emerald-300/70" :title="code.subject">{{ code.subject || '未提供邮件主题' }}</div>
            </div>
            <div class="mailbox-code-stats grid grid-cols-2 gap-2 text-xs">
              <div class="rounded-xl bg-slate-50 px-3 py-2.5 dark:bg-slate-800/70"><span class="block text-[10px] text-slate-400">收件数量</span><strong class="mt-0.5 block">{{ codeMailbox?.receive_count || 0 }} 封</strong></div>
              <div class="rounded-xl bg-slate-50 px-3 py-2.5 dark:bg-slate-800/70"><span class="block text-[10px] text-slate-400">收件时间</span><strong class="mt-0.5 block truncate" :title="formatTime(code.received_at)">{{ formatTime(code.received_at) }}</strong></div>
            </div>
            <button class="primary-button mailbox-code-copy w-full" @click="copyCode"><Clipboard :size="15" />复制验证码</button>
          </div>

          <div v-else class="mailbox-code-state flex min-h-44 flex-col items-center justify-center gap-3 text-center">
            <span class="flex h-12 w-12 items-center justify-center rounded-2xl bg-rose-50 text-rose-600 dark:bg-rose-950/50 dark:text-rose-300"><KeyRound :size="23" /></span>
            <div><strong class="block text-sm">暂未获取到验证码</strong><span class="mt-1 block max-w-sm text-xs leading-5 text-slate-400">{{ codeError || '请稍后重新取码。' }}</span></div>
            <button class="secondary-button" @click="closeCodeDialog">关闭</button>
          </div>
        </div>
      </section>
    </div>

    <div v-if="selectedMessage" class="fixed inset-0 z-[60] !m-0 flex items-center justify-center bg-slate-950/65 p-3 backdrop-blur-[4px] sm:p-5" role="presentation" @click.stop>
      <article class="mail-message-dialog flex flex-col overflow-hidden rounded-2xl border border-slate-200 bg-white shadow-2xl dark:border-slate-700 dark:bg-slate-900" role="dialog" aria-modal="true" aria-labelledby="mail-message-title">
        <header class="flex items-start justify-between gap-4 border-b border-slate-200 px-4 py-3.5 dark:border-slate-700 sm:px-5">
          <div class="min-w-0"><div class="mb-1.5 flex flex-wrap items-center gap-1.5"><span class="rounded-md bg-sky-100 px-2 py-0.5 text-[10px] font-bold text-sky-700 dark:bg-sky-950/60 dark:text-sky-200">完整邮件</span><span v-if="selectedMessage.source" class="rounded-md bg-slate-100 px-2 py-0.5 text-[10px] font-bold text-slate-500 dark:bg-slate-800 dark:text-slate-300">{{ selectedMessage.source }}</span><span class="rounded-md bg-violet-100 px-2 py-0.5 text-[10px] font-bold text-violet-700 dark:bg-violet-950/60 dark:text-violet-200">{{ messageContentTypeLabel(selectedMessage) }}</span></div><h2 id="mail-message-title" class="break-words text-base font-black leading-6">{{ selectedMessage.subject || '无主题' }}</h2><p class="mt-1 break-all text-[11px] text-slate-400">{{ selectedMessage.from || '未知发件人' }}</p></div>
          <button class="icon-button h-8 w-8 rounded-lg" title="关闭完整邮件" @click="closeSelectedMessage"><X :size="17" /></button>
        </header>
        <div class="mail-message-meta flex items-center justify-between gap-3 border-b border-slate-100 bg-slate-50 px-4 py-2 text-[10px] text-slate-400 dark:border-slate-800 dark:bg-slate-950/50 sm:px-5">
          <span class="truncate">收件邮箱：{{ selected?.email }}</span>
          <div class="flex shrink-0 items-center gap-3"><div v-if="selectedMessageHasHTML" class="mail-message-view-switch"><button type="button" :class="{ active: messageViewMode !== 'text' }" @click="messageViewMode = 'html'">邮件视图</button><button type="button" :class="{ active: messageViewMode === 'text' }" @click="messageViewMode = 'text'">纯文本</button></div><time>{{ formatTime(selectedMessage.received_at) }}</time></div>
        </div>
        <div class="mail-message-content relative min-h-0 flex-1 bg-slate-100 dark:bg-slate-950">
          <div v-if="selectedMessageLoading" class="mail-message-loading"><LoaderCircle :size="23" class="animate-spin" /><span>正在加载完整邮件</span></div>
          <iframe v-if="showSelectedMessageHTML" class="mail-message-document" :srcdoc="selectedMessageHTMLDocument" sandbox="allow-popups allow-popups-to-escape-sandbox" title="HTML 邮件正文"></iframe>
          <pre v-else class="mail-message-plain">{{ selectedMessage.body || '这封邮件没有正文内容。' }}</pre>
        </div>
      </article>
    </div>
  </div>
</template>
