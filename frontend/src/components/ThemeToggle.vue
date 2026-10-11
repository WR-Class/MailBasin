<script setup>
import { Moon, Sun } from '@lucide/vue'

defineProps({
  dark: Boolean,
})

defineEmits(['toggle'])
</script>

<template>
  <button class="theme-toggle" title="切换主题" aria-label="切换明暗主题" @click="$emit('toggle')">
    <Moon v-if="dark" :size="17" />
    <Sun v-else :size="17" class="text-amber-500" />
  </button>
</template>
