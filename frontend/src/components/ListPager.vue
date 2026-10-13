<script setup>
// 列表分页控件，四个账号页和邮箱池共用。
//
// 控件形状、按钮顺序、无障碍标签都照搬邮箱池那一套，连样式类也复用全局的
// .mailbox-pagination —— 同一个应用里两套分页交互比没有分页更糟：使用者得学两遍，
// 而且两边会慢慢分叉。类名里的 mailbox 是历史包袱，不影响这里。
import { ChevronLeft, ChevronRight, ChevronsLeft, ChevronsRight } from '@lucide/vue'

const props = defineProps({
  page: { type: Number, required: true },
  totalPages: { type: Number, required: true },
  total: { type: Number, required: true },
  // 计数单位，例如「个账号」「个邮箱」。
  unit: { type: String, default: '个' },
  loading: { type: Boolean, default: false },
})

const emit = defineEmits(['change'])

function goTo(target) {
  const last = Math.max(1, props.totalPages)
  const next = Math.min(Math.max(1, target), last)
  if (next !== props.page) emit('change', next)
}
</script>

<template>
  <div class="mailbox-pagination">
    <span>第 {{ page }} / {{ totalPages }} 页　总 {{ total }} {{ unit }}</span>
    <div>
      <button type="button" class="secondary-button" :disabled="loading || page <= 1" title="跳转到首页" aria-label="跳转到首页" @click="goTo(1)"><ChevronsLeft :size="15" /></button>
      <button type="button" class="secondary-button" :disabled="loading || page <= 1" title="上一页" aria-label="上一页" @click="goTo(page - 1)"><ChevronLeft :size="15" /></button>
      <button type="button" class="secondary-button" :disabled="loading || page >= totalPages" title="下一页" aria-label="下一页" @click="goTo(page + 1)"><ChevronRight :size="15" /></button>
      <button type="button" class="secondary-button" :disabled="loading || page >= totalPages" title="跳转到末页" aria-label="跳转到末页" @click="goTo(totalPages)"><ChevronsRight :size="15" /></button>
    </div>
  </div>
</template>
