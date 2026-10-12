<script setup>
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { AlertCircle, Clipboard, Inbox, LoaderCircle, MailOpen, RefreshCw } from '@lucide/vue'
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
})

const loading = ref(false)
const refreshing = ref(false)
const inbox = ref(null)
const errorText = ref('')
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

async function copyCode(code) {
  if (!code) return
  try {
    await navigator.clipboard.writeText(code)
    success(`已复制验证码 ${code}`)
  } catch {
    showError('浏览器拒绝了剪贴板访问，请手动选中复制')
  }
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
      <div>
        <h3><Inbox :size="15" />邮件与验证码</h3>
        <p v-if="accountName">{{ accountName }} 名下所有地址收到的信，都在这里</p>
        <p v-else>名下所有地址收到的信，都在这里</p>
      </div>
      <button type="button" class="secondary-button apple-check-button" :disabled="!accountId || loading || refreshing" @click="load({ refresh: true })">
        <LoaderCircle v-if="refreshing" :size="13" class="animate-spin" /><RefreshCw v-else :size="13" />重新读取
      </button>
    </header>

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
          <span v-if="mailbox.last_code" class="inbox-tag inbox-tag-code" :title="formatTime(mailbox.last_code_at)">最近验证码 {{ mailbox.last_code }}</span>
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
.inbox-message-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; max-height: 340px; overflow-y: auto; }
.inbox-message-row { display: flex; align-items: center; gap: 8px; padding: 6px 8px; border-radius: 8px; background: var(--surface-2, rgba(120, 120, 140, 0.08)); }
.inbox-message-main { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.inbox-message-subject { font-size: 12px; font-weight: 600; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.inbox-message-meta { display: flex; flex-wrap: wrap; gap: 4px; font-size: 11px; opacity: 0.7; }
.inbox-message-from { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 220px; }
.inbox-code-button { display: inline-flex; align-items: center; gap: 4px; padding: 3px 9px; border-radius: 7px; border: 1px solid rgba(16, 185, 129, 0.4); background: rgba(16, 185, 129, 0.12); font-size: 12px; font-weight: 600; cursor: pointer; }
.inbox-code-button:hover { background: rgba(16, 185, 129, 0.22); }
</style>
