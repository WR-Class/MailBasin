<script setup>
import { ArrowLeft, SearchX } from '@lucide/vue'
</script>

<template>
  <main class="flex min-h-screen items-center justify-center bg-slate-50 p-6 text-center dark:bg-slate-950"><div><div class="mx-auto flex h-20 w-20 items-center justify-center rounded-3xl bg-slate-100 text-slate-400 dark:bg-slate-800"><SearchX :size="34" /></div><div class="mt-6 text-5xl font-black text-slate-900 dark:text-white">404</div><h1 class="mt-3 text-xl font-bold">页面不存在</h1><p class="mt-2 text-sm text-slate-400">请检查访问地址，或返回控制台。</p><RouterLink to="/" class="primary-button mt-6"><ArrowLeft :size="17" />返回控制台</RouterLink></div></main>
</template>
