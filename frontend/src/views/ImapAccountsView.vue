<script setup>
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { AlertCircle, CheckCircle2, FileUp, Inbox, KeyRound, LoaderCircle, MailOpen, Plus, RefreshCw, Server, Trash2, X } from '@lucide/vue'
import { api } from '../api/client'
import { useConfirm } from '../composables/useConfirm'
import { subscribeRealtime } from '../composables/useRealtime'
import { useToast } from '../composables/useToast'
import AccountInboxPanel from '../components/AccountInboxPanel.vue'

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
    data.value = await api(`/api/imap-accounts?preset=${encodeURIComponent(preset.value)}`)
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

function selectAccount(account) {
  selected.value = account
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
  data.value = { items: [], module: {} }
  load()
})

onBeforeUnmount(() => {
  window.clearTimeout(realtimeRefreshTimer)
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
        </div>
      </header>

      <div v-if="moduleInfo.limit" class="apple-module-note">
        <AlertCircle :size="14" />
        <span>{{ moduleInfo.limit }}</span>
      </div>

      <div v-if="loading" class="apple-loading"><LoaderCircle :size="16" class="animate-spin" />正在加载{{ presetLabel }}</div>
      <div v-else class="apple-account-body">
        <section class="apple-account-list">
          <header class="apple-section-heading"><div><h3>{{ presetLabel }}</h3><p>选中一个后可在右侧查看它名下的地址</p></div><span>{{ accounts.length }} 个</span></header>
          <div v-if="!accounts.length" class="apple-empty"><span><Server :size="20" /></span><strong>还没有 {{ presetLabel }}</strong><small>点击“添加{{ presetLabel }}”，填入邮箱地址和客户端授权码。</small></div>
          <div v-else class="apple-account-table-viewport">
            <table class="apple-account-table">
              <colgroup><col class="apple-col-account" /><col class="apple-col-status" /><col class="apple-col-channel" /><col class="apple-col-action" /></colgroup>
              <thead><tr><th>邮箱</th><th>状态</th><th>地址</th><th>操作</th></tr></thead>
              <tbody>
                <tr v-for="account in accounts" :key="account.id" class="apple-account-row" :class="{ 'apple-account-row-selected': selected?.id === account.id }" tabindex="0" :aria-label="`查看 ${account.label || account.apple_id} 的地址`" @click="selectAccount(account)" @keydown.enter="selectAccount(account)" @keydown.space.prevent="selectAccount(account)">
                  <td><button type="button" class="apple-account-select" @click.stop="selectAccount(account)"><span class="apple-account-icon"><MailOpen :size="14" /></span><span><strong>{{ account.label || account.apple_id }}</strong><small>{{ account.apple_id }}</small></span></button></td>
                  <td><span :class="statusClass(account)" class="apple-status-badge">{{ statusLabel(account.icloud_status || account.status) }}</span></td>
                  <td><div class="apple-channel-list"><span class="apple-channel-pill"><Inbox :size="11" /><span>{{ account.mailbox_count || 0 }} 个地址</span><em v-if="account.pool_count" class="bg-sky-50 text-sky-600 dark:bg-sky-950/30 dark:text-sky-300">其中 {{ account.pool_count }} 个进池</em></span></div></td>
                  <td><button type="button" class="apple-delete-button" :title="`删除 ${account.label || account.apple_id}`" :aria-label="`删除 ${account.label || account.apple_id}`" :disabled="isBusy(`delete:${account.id}`)" @click.stop="deleteAccount(account)"><LoaderCircle v-if="isBusy(`delete:${account.id}`)" :size="13" class="animate-spin" /><Trash2 v-else :size="13" /></button></td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>

        <aside class="apple-detail-panel">
          <div v-if="!selected" class="apple-detail-empty"><span><MailOpen :size="20" /></span><strong>选择一个邮箱</strong><small>它的服务器信息与名下地址会显示在这里。</small></div>
          <template v-else>
            <header class="apple-detail-heading"><div><span>所选邮箱</span><h3>{{ selected.label || selected.apple_id }}</h3><p>{{ selected.apple_id }}</p></div><button class="secondary-button apple-check-button" :disabled="isBusy(`sync:${selected.id}`)" @click="syncAccount()"><LoaderCircle v-if="isBusy(`sync:${selected.id}`)" :size="13" class="animate-spin" /><RefreshCw v-else :size="13" />{{ isBusy(`sync:${selected.id}`) ? '同步中' : '立即同步' }}</button></header>
            <div class="ms-detail-scroll">
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

            <div class="apple-detail-block">
              <header class="apple-section-heading"><div><h3>名下地址</h3><p>默认不进邮箱池，需要时逐个勾选；勾选后邮箱池里就能看到</p></div><button class="secondary-button apple-check-button" @click="openAddAddressDialog"><Plus :size="13" />添加地址</button></header>
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

            <div class="apple-detail-block">
              <header class="apple-section-heading"><div><h3>服务商说明</h3><p>这些限制来自服务商本身，不是本程序的选择</p></div></header>
              <dl class="apple-meta-list">
                <div><dt>IMAP 服务器</dt><dd class="font-mono">{{ selected.imap_host }}:{{ selected.imap_port || 993 }}</dd></div>
                <div><dt>地址数量</dt><dd>{{ selected.mailbox_count || 0 }} 个，其中 {{ selected.pool_count || 0 }} 个在邮箱池</dd></div>
                <div><dt>已收邮件</dt><dd>{{ selected.message_count || 0 }} 封</dd></div>
              </dl>
              <p v-if="moduleInfo.hint" class="apple-detail-hint"><AlertCircle :size="13" />{{ moduleInfo.hint }}</p>
            </div>
            <AccountInboxPanel :account-id="selected?.id || ''" :account-name="selected?.label || selected?.apple_id || ''" />
            </div>
          </template>
        </aside>
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
