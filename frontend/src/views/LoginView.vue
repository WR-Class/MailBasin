<script setup>
import { computed, ref } from 'vue'
import { Cloud, Eye, EyeOff, KeyRound, LoaderCircle, LockKeyhole, ShieldCheck, User } from '@lucide/vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuth } from '../composables/useAuth'
import { useToast } from '../composables/useToast'

const route = useRoute()
const router = useRouter()
const { authState, login } = useAuth()
const username = ref('admin')
const password = ref('')
const confirmPassword = ref('')
const showPassword = ref(false)
const showConfirmPassword = ref(false)
const loading = ref(false)
const { error: showError } = useToast()

const setupRequired = computed(() => authState.setupRequired)
const title = computed(() => setupRequired.value ? '设置本地管理员' : '欢迎回来')
const submitText = computed(() => setupRequired.value ? '创建管理员并进入' : '登录控制台')

async function submit() {
  showConfirmPassword.value = false
  if (setupRequired.value && password.value !== confirmPassword.value) {
    showError('两次输入的密码不一致')
    return
  }
  loading.value = true
  try {
    await login(username.value, password.value, setupRequired.value)
    const redirect = typeof route.query.redirect === 'string' ? route.query.redirect : '/'
    await router.replace(redirect)
  } catch (err) {
    showError(err.message)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <main class="relative flex min-h-screen items-center justify-center overflow-hidden bg-slate-50 px-4 py-10 dark:bg-slate-950">
    <div class="pointer-events-none absolute inset-0 overflow-hidden"><div class="absolute -left-28 -top-28 h-96 w-96 rounded-full bg-emerald-200/30 blur-3xl dark:bg-emerald-900/20" /><div class="absolute -bottom-40 -right-32 h-[30rem] w-[30rem] rounded-full bg-sky-200/30 blur-3xl dark:bg-sky-900/20" /></div>
    <section class="relative grid w-full max-w-5xl overflow-hidden rounded-3xl border border-white/80 bg-white shadow-2xl shadow-slate-200/60 dark:border-slate-800 dark:bg-slate-900 dark:shadow-black/20 lg:grid-cols-[1.08fr_0.92fr]">
      <div class="hidden min-h-[620px] flex-col justify-between bg-gradient-to-br from-emerald-500 via-emerald-600 to-teal-700 p-10 text-white lg:flex">
        <div class="flex items-center gap-3"><div class="rounded-2xl bg-white/15 p-3 backdrop-blur"><Cloud :size="28" /></div><div><div class="text-lg font-black">iCloud Privacy Mail</div><div class="text-xs tracking-[0.18em] text-emerald-100">隐私邮箱控制台</div></div></div>
        <div>
          <div class="mb-6 inline-flex items-center gap-2 rounded-full bg-white/10 px-3 py-1.5 text-xs font-semibold backdrop-blur"><ShieldCheck :size="15" />单用户本地模式</div>
          <h1 class="max-w-md text-4xl font-black leading-tight">统一管理账号，<br />高效使用隐私邮箱。</h1>
          <p class="mt-5 max-w-md text-sm leading-7 text-emerald-50/85">集中管理 Apple 账号、隐私邮箱、邮件验证码和自动创建任务。</p>
        </div>
        <div class="grid grid-cols-3 gap-3 text-xs"><div class="rounded-xl bg-white/10 p-3 backdrop-blur"><div class="font-bold">Go 接口</div><div class="mt-1 text-emerald-100">模块化后端</div></div><div class="rounded-xl bg-white/10 p-3 backdrop-blur"><div class="font-bold">Vue 3</div><div class="mt-1 text-emerald-100">响应式前端</div></div><div class="rounded-xl bg-white/10 p-3 backdrop-blur"><div class="font-bold">本地运行</div><div class="mt-1 text-emerald-100">本机优先</div></div></div>
      </div>

      <div class="flex min-h-[560px] items-center p-6 sm:p-10 lg:p-12">
        <form class="w-full" @submit.prevent="submit">
          <div class="mb-8 lg:hidden"><div class="mb-5 flex h-12 w-12 items-center justify-center rounded-2xl bg-emerald-50 text-emerald-500 dark:bg-emerald-950/40"><Cloud :size="25" /></div></div>
          <div class="text-xs font-bold tracking-[0.2em] text-emerald-500">{{ setupRequired ? '首次运行' : '安全登录' }}</div>
          <h2 class="mt-2 text-3xl font-black text-slate-900 dark:text-white">{{ title }}</h2>
          <p class="mt-3 text-sm leading-6 text-slate-500 dark:text-slate-400">{{ setupRequired ? '这是第一次启动，请创建唯一的本地管理员。' : '输入本地管理员账号后进入控制台。' }}</p>

          <div class="mt-8 space-y-5">
            <label class="block"><span class="mb-2 block text-sm font-bold text-slate-700 dark:text-slate-300">账号</span><div class="relative"><User :size="17" class="pointer-events-none absolute left-3.5 top-1/2 -translate-y-1/2 text-slate-400" /><input v-model.trim="username" class="field field-leading" autocomplete="username" placeholder="admin" required /></div></label>
            <label class="block"><span class="mb-2 block text-sm font-bold text-slate-700 dark:text-slate-300">密码</span><div class="relative"><KeyRound :size="17" class="pointer-events-none absolute left-3.5 top-1/2 -translate-y-1/2 text-slate-400" /><input v-model="password" class="field field-leading field-trailing" :type="showPassword ? 'text' : 'password'" autocomplete="current-password" placeholder="至少 8 位" required /><button type="button" class="absolute right-3 top-1/2 -translate-y-1/2 rounded-lg p-1 text-slate-400 hover:text-slate-600" title="显示或隐藏密码" @click="showPassword = !showPassword"><EyeOff v-if="showPassword" :size="17" /><Eye v-else :size="17" /></button></div></label>
            <label v-if="setupRequired" class="block"><span class="mb-2 block text-sm font-bold text-slate-700 dark:text-slate-300">确认密码</span><div class="relative"><LockKeyhole :size="17" class="pointer-events-none absolute left-3.5 top-1/2 -translate-y-1/2 text-slate-400" /><input v-model="confirmPassword" class="field field-leading field-trailing" :type="showConfirmPassword ? 'text' : 'password'" autocomplete="new-password" placeholder="再次输入密码" required /><button type="button" class="absolute right-3 top-1/2 -translate-y-1/2 rounded-lg p-1 text-slate-400 hover:text-slate-600 dark:hover:text-slate-200" :title="showConfirmPassword ? '隐藏确认密码' : '显示确认密码'" @click="showConfirmPassword = !showConfirmPassword"><EyeOff v-if="showConfirmPassword" :size="17" /><Eye v-else :size="17" /></button></div></label>
          </div>

          <button class="primary-button mt-7 w-full py-3" :disabled="loading"><LoaderCircle v-if="loading" :size="18" class="animate-spin" /><ShieldCheck v-else :size="18" />{{ loading ? '正在处理...' : submitText }}</button>
          <p class="mx-auto mt-5 max-w-sm text-center text-xs leading-5 text-slate-400">登录状态仅保存在安全 Cookie 中；公网部署时再启用 HTTPS 配置。</p>
        </form>
      </div>
    </section>
  </main>
</template>
