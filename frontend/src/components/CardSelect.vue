<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Check, ChevronDown } from '@lucide/vue'

defineOptions({ inheritAttrs: false })

const props = defineProps({
  modelValue: { type: [String, Number, Array], default: '' },
  options: { type: Array, default: () => [] },
  placeholder: { type: String, default: '请选择' },
  ariaLabel: { type: String, default: '选择选项' },
  multiple: { type: Boolean, default: false },
  compact: { type: Boolean, default: false },
  disabled: { type: Boolean, default: false },
})

const emit = defineEmits(['update:modelValue', 'change'])
const root = ref(null)
const menu = ref(null)
const open = ref(false)
const menuMaxHeight = ref('')

function sameValue(left, right) {
  return String(left) === String(right)
}

function isSelected(value) {
  if (props.multiple) return (Array.isArray(props.modelValue) ? props.modelValue : []).some((item) => sameValue(item, value))
  return sameValue(props.modelValue, value)
}

const selectedOptions = computed(() => props.options.filter((option) => isSelected(option.value)))
const selectedLabel = computed(() => {
  if (!selectedOptions.value.length) return props.placeholder
  if (!props.multiple || selectedOptions.value.length === 1) return selectedOptions.value[0].label
  return `已选择 ${selectedOptions.value.length} 项`
})

function selectOption(option) {
  if (props.disabled || option.disabled) return
  if (props.multiple) {
    const current = Array.isArray(props.modelValue) ? props.modelValue : []
    const next = isSelected(option.value)
      ? current.filter((item) => !sameValue(item, option.value))
      : [...current, option.value]
    emit('update:modelValue', next)
    emit('change', next)
    return
  }
  emit('update:modelValue', option.value)
  emit('change', option.value)
  open.value = false
}

async function updateMenuMaxHeight() {
  await nextTick()
  if (!menu.value) return
  const options = [...menu.value.querySelectorAll('.card-select-option')].slice(0, 5)
  if (!options.length) {
    menuMaxHeight.value = ''
    return
  }
  const lastOption = options[options.length - 1]
  const styles = window.getComputedStyle(menu.value)
  const paddingBottom = Number.parseFloat(styles.paddingBottom) || 0
  const borderTop = Number.parseFloat(styles.borderTopWidth) || 0
  const borderBottom = Number.parseFloat(styles.borderBottomWidth) || 0
  menuMaxHeight.value = `${Math.ceil(lastOption.offsetTop + lastOption.offsetHeight + paddingBottom + borderTop + borderBottom)}px`
}

async function toggleMenu() {
  if (props.disabled) return
  open.value = !open.value
  if (open.value) await updateMenuMaxHeight()
}

function closeOnOutside(event) {
  if (!root.value?.contains(event.target)) open.value = false
}

function closeOnEscape(event) {
  if (event.key === 'Escape') open.value = false
}

onMounted(() => {
  document.addEventListener('pointerdown', closeOnOutside, true)
  document.addEventListener('keydown', closeOnEscape)
})

watch(() => props.options, () => {
  if (open.value) updateMenuMaxHeight()
}, { deep: true })

onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', closeOnOutside, true)
  document.removeEventListener('keydown', closeOnEscape)
})
</script>

<template>
  <div ref="root" class="card-select" v-bind="$attrs">
    <button type="button" class="field card-select-trigger" :class="compact ? 'card-select-trigger-compact' : ''" :disabled="disabled" :aria-label="ariaLabel" :aria-expanded="open" :aria-multiselectable="multiple || undefined" aria-haspopup="listbox" :title="selectedLabel" @click="toggleMenu">
      <span class="truncate" :class="selectedOptions.length ? '' : 'text-slate-400'">{{ selectedLabel }}</span>
      <ChevronDown :size="14" class="shrink-0 text-slate-400 transition-transform" :class="open ? 'rotate-180' : ''" />
    </button>
    <div v-if="open" ref="menu" class="card-select-menu" :style="{ maxHeight: menuMaxHeight || undefined }" role="listbox" :aria-label="ariaLabel" :aria-multiselectable="multiple || undefined">
      <button v-for="option in options" :key="String(option.value)" type="button" class="card-select-option" :class="isSelected(option.value) ? 'card-select-option-selected' : ''" :disabled="option.disabled" :aria-selected="isSelected(option.value)" role="option" :title="option.label" @click="selectOption(option)">
        <span class="card-select-dot" :class="option.dot || (isSelected(option.value) ? 'bg-emerald-500' : 'bg-slate-400')" />
        <span class="min-w-0 flex-1"><strong class="block truncate">{{ option.label }}</strong><small v-if="option.description" class="mt-0.5 block truncate text-[9px] leading-3 font-normal text-slate-400">{{ option.description }}</small></span>
        <Check v-if="isSelected(option.value)" :size="13" class="shrink-0" />
      </button>
      <div v-if="!options.length" class="px-3 py-4 text-center text-xs text-slate-400">暂无可选项</div>
    </div>
  </div>
</template>
