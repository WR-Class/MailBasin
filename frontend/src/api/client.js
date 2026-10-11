export class ApiError extends Error {
  constructor(message, status = 0, code = '') {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

export async function api(path, options = {}) {
  const headers = new Headers(options.headers || {})
  if (options.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  let response
  try {
    response = await fetch(path, {
      credentials: 'same-origin',
      cache: path.startsWith('/api/') ? 'no-store' : 'default',
      ...options,
      headers,
    })
  } catch (error) {
    if (error?.name === 'AbortError') throw new ApiError('请求已取消')
    throw new ApiError('连接服务器失败，请检查网络或稍后重试')
  }
  let payload = null
  try {
    payload = await response.json()
  } catch {
    throw new ApiError(`服务器返回了无效响应（HTTP ${response.status}）`, response.status)
  }
  if (!response.ok || payload?.success === false) {
	if (response.status === 401 && path !== '/api/auth/login' && path !== '/api/auth/setup' && !path.startsWith('/api/v1/')) {
	  const redirect = `${window.location.pathname}${window.location.search}`
	  window.location.replace(`/login?redirect=${encodeURIComponent(redirect)}`)
	}
    throw new ApiError(payload?.message || `请求失败（HTTP ${response.status}）`, response.status, payload?.code || '')
  }
  return payload?.data ?? payload
}
