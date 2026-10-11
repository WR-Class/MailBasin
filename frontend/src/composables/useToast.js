import { ref } from 'vue'

const notices = ref([])
const timers = new Map()
let nextId = 1

function remove(id) {
  const timer = timers.get(id)
  if (timer) clearTimeout(timer)
  timers.delete(id)
  notices.value = notices.value.filter((item) => item.id !== id)
}

function push(text, type = 'success', duration = 5000) {
  if (!text) return null
  const id = nextId++
  notices.value = [...notices.value, { id, text: String(text), type, persistent: duration <= 0 }]
  while (notices.value.length > 3) {
    const removable = notices.value.find((item) => !item.persistent) || notices.value[0]
    remove(removable.id)
  }
  if (duration > 0) timers.set(id, window.setTimeout(() => remove(id), duration))
  return id
}

function update(id, text, type = 'success', duration = 5000) {
  if (!text) return id
  if (!notices.value.some((item) => item.id === id)) return push(text, type, duration)
  const timer = timers.get(id)
  if (timer) clearTimeout(timer)
  timers.delete(id)
  notices.value = notices.value.map((item) => item.id === id
    ? { ...item, text: String(text), type, persistent: duration <= 0 }
    : item)
  if (duration > 0) timers.set(id, window.setTimeout(() => remove(id), duration))
  return id
}

export function useToast() {
  return {
    notices,
    push,
    success: (text, duration = 5000) => push(text, 'success', duration),
    error: (text, duration = 5000) => push(text, 'error', duration),
    info: (text, duration = 5000) => push(text, 'info', duration),
    warning: (text, duration = 5000) => push(text, 'warning', duration),
    update,
    dismiss: remove,
  }
}
