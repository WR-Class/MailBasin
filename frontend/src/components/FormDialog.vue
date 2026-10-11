<script setup>
import { LoaderCircle, Save, X } from '@lucide/vue'

defineProps({
  open: { type: Boolean, default: false },
  title: { type: String, required: true },
  description: { type: String, default: '' },
  busy: { type: Boolean, default: false },
  submitText: { type: String, default: '保存' },
})

const emit = defineEmits(['close', 'submit'])
</script>

<template>
  <div v-if="open" class="fixed inset-0 z-[70] !m-0 flex items-center justify-center bg-slate-950/55 p-4 backdrop-blur-[3px]" role="presentation" @click.self="!busy && emit('close')">
    <section class="panel w-full max-w-md overflow-visible p-0 shadow-2xl" role="dialog" aria-modal="true" :aria-label="title">
      <header class="flex items-start justify-between gap-4 border-b border-slate-200 px-5 py-4 dark:border-slate-700">
        <div class="min-w-0">
          <h2 class="font-black">{{ title }}</h2>
          <p v-if="description" class="mt-1 truncate text-xs text-slate-400">{{ description }}</p>
        </div>
        <button type="button" class="icon-button h-8 w-8 rounded-lg" title="关闭弹窗" :disabled="busy" @click="emit('close')"><X :size="17" /></button>
      </header>

      <form @submit.prevent="emit('submit')">
        <div class="p-5"><slot /></div>
        <footer class="flex justify-end gap-2 border-t border-slate-200 bg-slate-50 px-5 py-3 dark:border-slate-700 dark:bg-slate-900/50">
          <button type="button" class="secondary-button px-3.5 py-2 text-xs" :disabled="busy" @click="emit('close')">取消</button>
          <button type="submit" class="primary-button px-3.5 py-2 text-xs" :disabled="busy"><LoaderCircle v-if="busy" :size="14" class="animate-spin" /><Save v-else :size="14" />{{ busy ? '保存中' : submitText }}</button>
        </footer>
      </form>
    </section>
  </div>
</template>
