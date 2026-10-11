import { reactive, readonly } from 'vue'

const state = reactive({
  visible: false,
  id: 0,
  title: '请确认操作',
  message: '',
  confirmText: '确定',
  cancelText: '取消',
  tone: 'primary',
})

let resolver = null

function close(result) {
  if (!state.visible) return
  const resolve = resolver
  resolver = null
  state.visible = false
  resolve?.(result)
}

function confirm(options) {
  const config = typeof options === 'string' ? { message: options } : (options || {})
  if (resolver) resolver(false)

  Object.assign(state, {
    visible: true,
    id: state.id + 1,
    title: config.title || '请确认操作',
    message: config.message || '',
    confirmText: config.confirmText || '确定',
    cancelText: config.cancelText || '取消',
    tone: config.tone || 'primary',
  })

  return new Promise((resolve) => {
    resolver = resolve
  })
}

export function useConfirm() {
  return {
    confirmState: readonly(state),
    confirm,
    accept: () => close(true),
    cancel: () => close(false),
  }
}
