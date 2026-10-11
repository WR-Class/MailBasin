<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Database, Download, FileJson, Globe2, KeyRound, Mail, ShieldAlert } from '@lucide/vue'
import { api } from '../api/client'
import { subscribeRealtime } from '../composables/useRealtime'
import { useToast } from '../composables/useToast'

const runtime = ref({})
const settings = ref({})
const dataPath = ref('')
const { error: showError } = useToast()
let realtimeRefreshTimer
let realtimeUnsubscribe = () => {}

const publicAPIKeyReady = computed(() => Boolean(String(settings.value.public_api_key || '').trim() || runtime.value.config_api_key_configured))
const publicAPIKeySource = computed(() => {
  if (String(settings.value.public_api_key || '').trim()) return '系统设置'
  if (runtime.value.config_api_key_configured) return 'config.json'
  return '尚未设置'
})

const exportItems = [
  { title: '运行数据', description: '导出账号、邮箱、登录态和系统设置，不包含本地邮件正文。', href: '/api/runtime/export', icon: FileJson, tone: 'text-violet-600 bg-violet-100 dark:bg-violet-950/60 dark:text-violet-300', format: 'JSON' },
  { title: '运行数据与邮件', description: '在完整运行数据中加入所有已同步的本地邮件内容。', href: '/api/runtime/export?include_messages=1', icon: Database, tone: 'text-sky-600 bg-sky-100 dark:bg-sky-950/60 dark:text-sky-300', format: 'JSON' },
  { title: '邮箱地址', description: '只导出邮箱池中的隐私邮箱地址，方便导入其他本地工具。', href: '/api/runtime/export-mailbox-emails?format=txt', icon: Mail, tone: 'text-emerald-600 bg-emerald-100 dark:bg-emerald-950/60 dark:text-emerald-300', format: 'TXT' },
  { title: '取码 API', description: '导出每个邮箱的地址与独立取码 API 链接。', href: '/api/runtime/export-mailbox-apis?format=txt', icon: KeyRound, tone: 'text-amber-600 bg-amber-100 dark:bg-amber-950/60 dark:text-amber-300', format: 'TXT' },
]

async function load() {
  try {
    const data = await api('/api/settings')
    runtime.value = data.runtime || {}
    settings.value = data.settings || {}
    dataPath.value = data.data_path || ''
  } catch (err) {
    showError(err.message)
  }
}

function scheduleRealtimeRefresh() {
  window.clearTimeout(realtimeRefreshTimer)
  realtimeRefreshTimer = window.setTimeout(load, 120)
}

onMounted(() => {
  load()
  realtimeUnsubscribe = subscribeRealtime('settings', scheduleRealtimeRefresh)
})

onBeforeUnmount(() => {
  window.clearTimeout(realtimeRefreshTimer)
  realtimeUnsubscribe()
})
</script>

<template>
  <div class="exports-page">
    <section class="panel exports-workbench">
      <header class="exports-command-bar"><div><span><Download :size="16" /></span><div><h2>本地导出</h2><p>下载运行数据、邮件、邮箱地址或取码 API</p></div></div><span class="exports-warning"><ShieldAlert :size="12" />包含敏感本地数据</span></header>
      <div class="exports-item-grid"><a v-for="item in exportItems" :key="item.title" :href="item.href" download class="exports-item"><span :class="item.tone" class="exports-item-icon"><component :is="item.icon" :size="16" /></span><span class="exports-item-copy"><span><strong>{{ item.title }}</strong><em>{{ item.format }}</em></span><small>{{ item.description }}</small></span><span class="exports-download"><Download :size="13" />下载</span></a></div>
    </section>

    <section class="panel exports-environment">
      <header><div><Globe2 :size="14" /><span><strong>导出环境</strong><small>当前服务生成文件时使用的运行信息</small></span></div></header>
      <div class="exports-environment-grid"><div><span>SQLite 数据库</span><strong class="font-mono" :title="dataPath">{{ dataPath || '使用当前运行数据库' }}</strong></div><div><span>公共基础地址</span><strong>{{ runtime.public_base_url || '按当前访问地址生成' }}</strong></div><div><span>全局 API Key</span><strong :class="publicAPIKeyReady ? 'text-emerald-600 dark:text-emerald-300' : 'text-amber-600 dark:text-amber-300'">{{ publicAPIKeyReady ? `已配置（${publicAPIKeySource}）` : '尚未设置' }}</strong></div></div>
      <footer>运行数据导出包含 Apple 登录态、Cookie 和 App 专用密码等内容，请将下载文件保存在可信位置。</footer>
    </section>
  </div>
</template>
