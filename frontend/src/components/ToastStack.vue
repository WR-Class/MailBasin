<script setup>
import { CheckCircle2, CircleAlert, Info, X } from '@lucide/vue'
import { useToast } from '../composables/useToast'

const { notices, dismiss } = useToast()

const iconMap = { success: CheckCircle2, error: CircleAlert, info: Info, warning: CircleAlert }

function iconFor(type) {
  return iconMap[type] || Info
}

function noticeClass(type) {
  return {
    success: 'border-emerald-200 bg-white text-emerald-700 shadow-emerald-500/10 dark:border-emerald-900 dark:bg-slate-800 dark:text-emerald-300',
    error: 'border-rose-200 bg-white text-rose-700 shadow-rose-500/10 dark:border-rose-900 dark:bg-slate-800 dark:text-rose-300',
    info: 'border-sky-200 bg-white text-sky-700 shadow-sky-500/10 dark:border-sky-900 dark:bg-slate-800 dark:text-sky-300',
    warning: 'border-amber-200 bg-white text-amber-700 shadow-amber-500/10 dark:border-amber-900 dark:bg-slate-800 dark:text-amber-300',
  }[type] || 'border-slate-200 bg-white text-slate-700 shadow-slate-500/10 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-200'
}
</script>

<template>
  <div class="pointer-events-none fixed inset-x-0 top-4 z-[100] flex justify-center px-4 sm:top-5" aria-live="polite" aria-atomic="false">
    <TransitionGroup name="toast" tag="div" class="flex w-full max-w-xl flex-col gap-2">
      <div v-for="notice in notices" :key="notice.id" :class="noticeClass(notice.type)" class="pointer-events-auto flex min-h-14 items-center gap-3 rounded-2xl border px-4 py-3 text-sm font-semibold shadow-xl backdrop-blur-sm">
        <component :is="iconFor(notice.type)" :size="18" class="shrink-0" />
        <span class="min-w-0 flex-1 break-words leading-5">{{ notice.text }}</span>
        <button class="icon-button -mr-1 h-8 w-8" title="关闭提示" @click="dismiss(notice.id)"><X :size="15" /></button>
      </div>
    </TransitionGroup>
  </div>
</template>
