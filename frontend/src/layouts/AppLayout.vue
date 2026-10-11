<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Activity, ChevronDown, LogOut, Menu, UserRound } from '@lucide/vue'
import { useRoute, useRouter } from 'vue-router'
import Sidebar from '../components/Sidebar.vue'
import AnnouncementCenter from '../components/AnnouncementCenter.vue'
import ThemeToggle from '../components/ThemeToggle.vue'
import { useAuth } from '../composables/useAuth'
import { connectRealtime, disconnectRealtime, realtimeState } from '../composables/useRealtime'

const route = useRoute()
const router = useRouter()
const { authState, logout } = useAuth()
const sidebarOpen = ref(false)
const dark = ref(false)
const profileOpen = ref(false)

const title = computed(() => route.meta.title || '控制台')
const subtitle = computed(() => route.meta.subtitle || '')
const realtimeText = computed(() => ({
  connected: '实时已连接',
  connecting: '实时连接中',
  reconnecting: '实时重连中',
  closed: '实时已断开',
}[realtimeState.status] || '实时状态未知'))

function applyTheme(value) {
  dark.value = value
  document.documentElement.classList.toggle('dark', value)
  localStorage.setItem('ipm_v2_theme', value ? 'dark' : 'light')
}

function handleKeydown(event) {
  if (event.key === 'Escape') {
    profileOpen.value = false
    sidebarOpen.value = false
  }
}

async function signOut() {
  try {
    await logout()
  } finally {
    disconnectRealtime()
    profileOpen.value = false
    router.replace({ name: 'login' })
  }
}

onMounted(() => {
  const saved = localStorage.getItem('ipm_v2_theme')
  applyTheme(saved ? saved === 'dark' : window.matchMedia('(prefers-color-scheme: dark)').matches)
  window.addEventListener('keydown', handleKeydown)
  connectRealtime()
})

onBeforeUnmount(() => {
  disconnectRealtime()
  window.removeEventListener('keydown', handleKeydown)
})
</script>

<template>
  <div class="app-shell">
    <Sidebar :open="sidebarOpen" :dark="dark" @close="sidebarOpen = false" @toggle-theme="applyTheme(!dark)" />
    <div class="main-shell">
      <header class="topbar">
        <div class="topbar-heading">
          <button class="mobile-nav-button" title="打开导航" aria-label="打开导航" @click="sidebarOpen = true"><Menu :size="19" /></button>
          <div class="topbar-title">
            <div class="breadcrumb"><span>控制台</span><b>/</b><strong>{{ title }}</strong></div>
            <p>{{ subtitle }}</p>
          </div>
        </div>
        <div class="topbar-actions">
          <span class="realtime-mode" :class="`realtime-${realtimeState.status}`" :title="realtimeText"><i />{{ realtimeText }}</span>
          <span class="local-mode"><Activity :size="14" />本地模式</span>
          <ThemeToggle :dark="dark" @toggle="applyTheme(!dark)" />
          <AnnouncementCenter />
          <div class="profile-menu">
            <button class="profile-trigger" :aria-expanded="profileOpen" aria-label="打开管理员菜单" @click="profileOpen = !profileOpen"><span class="admin-avatar"><UserRound :size="16" /></span><span>{{ authState.admin?.username || '管理员' }}</span><ChevronDown :size="14" /></button>
            <div v-if="profileOpen" class="profile-dropdown">
              <div><strong>{{ authState.admin?.username || '管理员' }}</strong><small>单用户本地控制台</small></div>
              <button @click="signOut"><LogOut :size="15" />退出登录</button>
            </div>
          </div>
        </div>
      </header>
      <main class="page-scroll">
        <RouterView v-slot="{ Component }">
          <Transition name="page" mode="out-in"><component :is="Component" /></Transition>
        </RouterView>
      </main>
    </div>
  </div>
</template>
