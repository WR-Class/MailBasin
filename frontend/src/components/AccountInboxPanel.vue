<script setup>
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { AlertCircle, ChevronRight, Clipboard, Inbox, LoaderCircle, MailOpen, RefreshCw } from '@lucide/vue'
import { api } from '../api/client'
import { subscribeRealtime } from '../composables/useRealtime'
import { useToast } from '../composables/useToast'

// 母号收件箱面板。
//
// 它只接收一个母号标识，不关心当前页面是哪个服务商——iCloud、Microsoft、
// Google、QQ、163 五种母号的收件箱在结构上完全相同，所以这个组件在五个
// 页面上各挂一次，而不是每页复制一份。
//
// 它读的是本地已经存下的邮件：面板要能随时打开，取新信是各页面上
// 「同步」按钮的事。这里只负责看，不负责取。
const props = defineProps({
  accountId: { type: String, default: '' },
  accountName: { type: String, default: '' },
  // 折叠模式：给「详情搬进展开行」的那套页面用，默认收起。
  // 没开这个开关的页面，渲染结果与以前完全一致。
  collapsible: { type: Boolean, default: false },
})

const loading = ref(false)
const refreshing = ref(false)
const inbox = ref(null)
const errorText = ref('')
// 折叠模式下标题栏就是开关；非折叠模式永远展开。
const open = ref(!props.collapsible)
const inboxSubtitle = computed(() =>
  props.accountName ? `${props.accountName} 名下所有地址收到的信，都在这里` : '名下所有地址收到的信，都在这里'
)
const { success, error: showError } = useToast()
let realtimeRefreshTimer
let realtimeUnsubscribe = () => {}

const mailboxes = computed(() => inbox.value?.mailboxes || [])
const messages = computed(() => inbox.value?.messages || [])
const codeCount = computed(() => inbox.value?.code_count || 0)
const total = computed(() => inbox.value?.total || 0)
// 有验证码的排前面：打开这个面板十次有九次是为了找验证码。
const ordered = computed(() => {
  const withCode = messages.value.filter((item) => item.code)
  const without = messages.value.filter((item) => !item.code)
  return [...withCode, ...without]
})

function formatTime(value) {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime()) || date.getFullYear() < 2000) return '—'
  return date.toLocaleString('zh-CN', { hour12: false })
}

async function load(options = {}) {
  if (!props.accountId) {
    inbox.value = null
    return
  }
  if (options.refresh) refreshing.value = true
  else loading.value = true
  errorText.value = ''
  try {
    inbox.value = await api(`/api/accounts/${encodeURIComponent(props.accountId)}/inbox?limit=50`)
  } catch (err) {
    // 面板失败不该拖垮整个页面：只在自己这块显示原因。
    errorText.value = err.message
    inbox.value = null
  } finally {
    loading.value = false
    refreshing.value = false
  }
}

// 剪贴板 API 只在安全上下文里可用：localhost 算安全，局域网 IP 不算。
// 所以在它缺席或拒绝时退回 execCommand；两条路都失败就把话说清楚，
// 而不是点下去什么都不发生。
async function writeClipboard(text) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // 落到下面的兜底
    }
  }
  try {
    const area = document.createElement('textarea')
    area.value = text
    area.setAttribute('readonly', '')
    area.style.position = 'fixed'
    area.style.top = '-1000px'
    document.body.appendChild(area)
    try {
      // 先给焦点再选中：有些浏览器不这样就不让 execCommand 真的写入剪贴板。
      area.focus()
      area.select()
      area.setSelectionRange(0, text.length)
      return document.execCommand('copy')
    } finally {
      document.body.removeChild(area)
    }
  } catch {
    return false
  }
}

async function copyCode(code) {
  if (!code) return
  if (await writeClipboard(code)) success(`已复制验证码 ${code}`)
  else showError('浏览器拒绝了剪贴板访问，请手动选中复制')
}

function scheduleRealtimeRefresh(change) {
  if (change.resource !== 'mailbox' && change.resource !== 'message') return
  window.clearTimeout(realtimeRefreshTimer)
  realtimeRefreshTimer = window.setTimeout(() => load({ refresh: true }), 200)
}

// 切换母号时立刻重新取一次，不等实时事件。
watch(() => props.accountId, () => load())

onMounted(async () => {
  await load()
  realtimeUnsubscribe = subscribeRealtime(scheduleRealtimeRefresh)
})

onBeforeUnmount(() => {
  window.clearTimeout(realtimeRefreshTimer)
  realtimeUnsubscribe()
})
</script>

<template>
  <section class="panel apple-detail-block inbox-panel">
    <header class="apple-section-heading">
      <!-- 折叠模式下标题栏本身就是开关。标题右侧的「重新读取」必须是另一个
           兄弟按钮，不能嵌进开关里——按钮套按钮是无效结构。 -->
      <button v-if="collapsible" type="button" class="detail-section-toggle" :aria-expanded="open" @click="open = !open">
        <ChevronRight :size="15" class="detail-section-caret" />
        <span class="detail-section-title">
          <strong><Inbox :size="15" />邮件与验证码</strong>
          <small>{{ inboxSubtitle }}</small>
        </span>
      </button>
      <div v-else>
        <h3><Inbox :size="15" />邮件与验证码</h3>
        <p>{{ inboxSubtitle }}</p>
      </div>
      <button v-if="!collapsible || open" type="button" class="secondary-button apple-check-button" :disabled="!accountId || loading || refreshing" @click="load({ refresh: true })">
        <LoaderCircle v-if="refreshing" :size="13" class="animate-spin" /><RefreshCw v-else :size="13" />重新读取
      </button>
    </header>

    <template v-if="!collapsible || open">
      <div v-if="!accountId" class="apple-state-empty">先在左边选中一个邮箱。</div>
      <div v-else-if="loading" class="apple-state-empty"><LoaderCircle :size="14" class="animate-spin" />正在读取本地邮件</div>
      <div v-else-if="errorText" class="apple-state-empty"><AlertCircle :size="14" />{{ errorText }}</div>
      <template v-else>
        <div class="inbox-summary">
          <span class="inbox-summary-pill">共 {{ total }} 封</span>
          <span class="inbox-summary-pill inbox-summary-code">{{ codeCount }} 封带验证码</span>
          <span v-if="total > messages.length" class="inbox-summary-pill">只显示最近 {{ messages.length }} 封</span>
        </div>

        <div v-if="mailboxes.length" class="inbox-address-list">
          <article v-for="mailbox in mailboxes" :key="mailbox.id" class="inbox-address-row">
            <span class="inbox-address-email font-mono">{{ mailbox.email }}</span>
            <span v-if="!mailbox.pool_hidden" class="inbox-tag inbox-tag-pool">已在邮箱池</span>
            <span v-else class="inbox-tag">未进池</span>
            <button v-if="mailbox.last_code" type="button" class="inbox-tag inbox-tag-code inbox-tag-copy" :title="`复制验证码 ${mailbox.last_code}（收到于 ${formatTime(mailbox.last_code_at)}）`" @click="copyCode(mailbox.last_code)">
              <Clipboard :size="11" /><span>最近验证码</span><span class="font-mono inbox-tag-code-value">{{ mailbox.last_code }}</span>
            </button>
            <span v-else class="inbox-tag inbox-tag-muted">暂无验证码</span>
          </article>
        </div>

        <div v-if="!messages.length" class="apple-state-empty">名下地址还没有收到任何邮件。点页面上的「同步」去读一次。</div>
        <ul v-else class="inbox-message-list">
          <li v-for="message in ordered" :key="message.id" class="inbox-message-row">
            <div class="inbox-message-main">
              <strong class="inbox-message-subject">{{ message.subject || '（无标题）' }}</strong>
              <small class="inbox-message-meta">
                <span class="font-mono">{{ message.mailbox_email }}</span>
                <span>·</span>
                <span>{{ formatTime(message.received_at) }}</span>
                <span v-if="message.from">·</span>
                <span v-if="message.from" class="inbox-message-from">{{ message.from }}</span>
              </small>
            </div>
            <button v-if="message.code" type="button" class="inbox-code-button" :title="`复制验证码 ${message.code}`" @click="copyCode(message.code)">
              <Clipboard :size="12" /><span class="font-mono">{{ message.code }}</span>
            </button>
            <span v-else class="inbox-tag inbox-tag-muted">无验证码</span>
          </li>
        </ul>
        <p v-if="messages.length" class="apple-detail-hint"><AlertCircle :size="13" />这里读的是本地已存下的邮件，不发网络请求；要看新到的信，点页面上的「同步」。</p>
      </template>
    </template>
  </section>
</template>

<style scoped>
.inbox-panel { display: flex; flex-direction: column; gap: 10px; }
.inbox-summary { display: flex; flex-wrap: wrap; gap: 6px; }
.inbox-summary-pill { padding: 2px 8px; border-radius: 999px; font-size: 11px; background: var(--surface-2, rgba(120, 120, 140, 0.12)); }
.inbox-summary-code { background: rgba(16, 185, 129, 0.14); }
.inbox-address-list { display: flex; flex-direction: column; gap: 4px; }
.inbox-address-row { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; padding: 5px 8px; border-radius: 8px; background: var(--surface-2, rgba(120, 120, 140, 0.08)); }
.inbox-address-email { font-size: 12px; flex: 1 1 200px; overflow-wrap: anywhere; }
.inbox-tag { padding: 1px 7px; border-radius: 999px; font-size: 11px; background: rgba(120, 120, 140, 0.16); white-space: nowrap; }
.inbox-tag-pool { background: rgba(56, 189, 248, 0.18); }
.inbox-tag-code { background: rgba(16, 185, 129, 0.18); }
.inbox-tag-muted { opacity: 0.55; }
/* 「最近验证码」现在是个真按钮：一眼要能看出可以点。 */
.inbox-tag-copy { display: inline-flex; align-items: center; gap: 4px; border: 1px solid rgba(16, 185, 129, 0.45); color: inherit; font: inherit; cursor: pointer; }
.inbox-tag-copy:hover { background: rgba(16, 185, 129, 0.3); }
.inbox-tag-copy:active { transform: translateY(1px); }
.inbox-tag-code-value { font-weight: 700; letter-spacing: 0.04em; }
.inbox-message-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; max-height: 340px; overflow-y: auto; }
.inbox-message-row { display: flex; align-items: center; gap: 8px; padding: 6px 8px; border-radius: 8px; background: var(--surface-2, rgba(120, 120, 140, 0.08)); }
.inbox-message-main { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.inbox-message-subject { font-size: 12px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.inbox-message-meta { display: flex; flex-wrap: wrap; gap: 4px; font-size: 11px; opacity: 0.7; }
.inbox-message-from { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 220px; }
.inbox-code-button { display: inline-flex; align-items: center; gap: 4px; padding: 3px 9px; border-radius: 7px; border: 1px solid rgba(16, 185, 129, 0.4); background: rgba(16, 185, 129, 0.12); font-size: 12px; font-weight: 600; cursor: pointer; }
.inbox-code-button:hover { background: rgba(16, 185, 129, 0.22); }
</style>
