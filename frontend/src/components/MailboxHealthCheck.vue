<script setup>
// 账号页上的体检：把这一页列出的账号名下所有地址检查一遍，看还能不能读到信。
//
// 做成共用组件，是因为四个账号页要的是同一件事。复制四份的结果是四处各自漂移：
// 一处改了措辞或判定，另外三处还原地不动，而使用者看到的是同一个按钮。
//
// 范围由调用页给：传进来的是这一页列出的账号，组件自己去取这些账号名下的地址，
// 并且带上池外的那些——在进池之前就把读不了的筛掉，正是这个功能存在的理由，
// 而只看池内的话，被挡在池外的那批恰恰永远没人检查。
//
// 报告做成弹窗，不是工具条里展开的一行：那份报告可以列几十条读不了的地址，内嵌时会把
// 它所在的页面整个撑长，于是页面上多出一条滚动条，而邮箱/账号列表自己还有一条——
// 两条滚动条抢滚轮，往下翻的时候根本分不清在翻哪一层。弹窗把报告移出文档流，
// 页面高度不再随结果变化，报告自己滚自己的。
import { computed, ref, watch } from 'vue'
import { LoaderCircle, ShieldCheck, X } from '@lucide/vue'
import { api } from '../api/client'
import { useToast } from '../composables/useToast'

const props = defineProps({
  // accounts：这一页列出的账号，只需要有 id 字段。
  accounts: { type: Array, default: () => [] },
  // title：名目，例如「微软账号」。
  title: { type: String, default: '账号' },
  // level：结论的分辨率。account 表示名下地址共用母号的一份授权，结论是账号级的。
  level: { type: String, default: 'account' },
})

const emit = defineEmits(['completed'])
const { success, error: showError } = useToast()
const state = ref({ running: false, done: 0, total: 0, report: null })
// 本次覆盖了哪些账号、多少个地址。必须说出来：这几页是分页的，组件拿到的是当前页列出的
// 那些账号，不说的话使用者会以为它体检了全部——而「以为查过了」比「没查」更危险。
const scope = ref({ accounts: 0, mailboxes: 0 })
// dialogOpen 和 state.report 分开：关闭弹窗只是把报告收起来，不该丢掉刚跑完的结论，
// 再点按钮就能立刻看回来，而不必再体检一遍（那会重新连一次邮件服务器）。
const dialogOpen = ref(false)

const accountLevel = computed(() => props.level === 'account')
const buttonLabel = computed(() => accountLevel.value ? `体检${props.title}的邮箱能否读取` : `体检本页${props.title}能否读取`)
const buttonTitle = computed(() => accountLevel.value
  ? '检查本页每个账号的读信通道。这些账号的地址共用母号那一份授权，所以结论是账号级的：判定为授权被拒的，名下地址会被标成失败、移出邮箱池；只是连不上的标成「连不上」，只改显示、不动邮箱池'
  : '逐个登录检查本页列出的地址还能不能读信。判定为凭据失效的会被标成失败、移出邮箱池；只是连不上的标成「连不上」，只改显示、不动邮箱池')
const unreadable = computed(() => (state.value.report?.items || []).filter((row) => row.covered && !row.readable))

function runHealthCheck() {
  if (state.value.running) return
  void startHealthCheck()
}

async function startHealthCheck() {
  const ids = []
  try {
    state.value = { running: true, done: 0, total: 0, report: null }
    dialogOpen.value = true
    for (const account of props.accounts) {
      if (!account?.id) continue
      // include_hidden=1 是关键：池外的地址也要体检。默认只看池内，
      // 而那正是「读不了的已经被挡住」之后最容易没人再看的一批。
      const data = await api(`/api/mailboxes?account_id=${encodeURIComponent(account.id)}&include_hidden=1&page_size=200`)
      ids.push(...(data.items || []).map((item) => item.id))
    }
    if (!ids.length) {
      state.value = { running: false, done: 0, total: 0, report: null }
      scope.value = { accounts: props.accounts.filter((account) => account?.id).length, mailboxes: 0 }
      dialogOpen.value = false
      showError(`本页列出的${props.title}名下还没有邮箱`)
      return
    }
    scope.value = { accounts: props.accounts.filter((account) => account?.id).length, mailboxes: ids.length }
    state.value = { running: true, done: 0, total: ids.length, report: null }
    const items = []
    // 按小批循环发请求：一个长请求会被浏览器掐断，而且这期间界面上什么都看不到。
    for (let start = 0; start < ids.length; start += 20) {
      const batch = ids.slice(start, start + 20)
      const data = await api('/api/mailboxes/probe-batch', { method: 'POST', body: JSON.stringify({ ids: batch }) })
      items.push(...(data.items || []))
      state.value = { ...state.value, done: items.length }
    }
    // 接口返回的每个计数都只覆盖它那一个小批，所以汇总必须在合并之后重算：
    // 直接拿最后一批的数字当全部的数字，是这类界面最容易说出口的一句假话。
    const report = { items, checked: 0, readable: 0, unreadable: 0, not_covered: 0, missing: 0, never_checked: 0, unreadable_but_offered: 0, demoted: 0 }
    for (const item of items) {
      if (item.missing) report.missing += 1
      else if (!item.covered) report.not_covered += 1
      else if (item.readable) { report.checked += 1; report.readable += 1 }
      else { report.checked += 1; report.unreadable += 1 }
      if (item.never_checked) report.never_checked += 1
      if (item.offered_while_unreadable) report.unreadable_but_offered += 1
      if (item.demoted) report.demoted += 1
    }
    // 合并后重排：「读不了的排最前」这句话得对整张表成立，而不只是对每一批成立。
    report.items = [...items].sort((a, b) => healthCheckRank(a) - healthCheckRank(b))
    state.value = { running: false, done: ids.length, total: ids.length, report }
    if (report.unreadable) showError(`体检完成：${report.unreadable} 个读不了，已摘出邮箱池 ${report.demoted} 个`)
    else success(`体检完成：${report.readable} 个能读，没有发现读不了的`)
  } catch (err) {
    state.value = { running: false, done: 0, total: 0, report: null }
    showError(`体检失败：${err.message}`)
  } finally {
    // 前面的小批次可能已经写回状态，即使后续批次失败也要刷新列表。
    if (ids.length) emit('completed')
  }
}

function healthCheckRank(item) {
  if (item.covered && !item.readable) return 0
  if (item.missing || !item.covered) return 1
  return 2
}

// 弹窗里显示写回后的账号状态，措辞要和账号页上的徽章说得一致，
// 否则报告说「连不上」而徽章显示 unreachable，使用者没法把它们对上。
function accountStatusLabel(item) {
  return ({ active: '正常', partial: '部分失败', failed: '失败', unreachable: '连不上' })[item.account_status] || item.account_status
}

function closeHealthCheck() {
  dialogOpen.value = false
}

function handleKeydown(event) {
  if (dialogOpen.value && event.key === 'Escape') closeHealthCheck()
}

watch(dialogOpen, (open) => {
  if (open) window.addEventListener('keydown', handleKeydown)
  else window.removeEventListener('keydown', handleKeydown)
})
</script>

<template>
  <!-- display:contents 让按钮成为调用页工具条的成员（跟其它按钮排在一行）。
       报告不再往工具条里塞东西，所以这里没有会撑高页面的兄弟节点。 -->
  <div style="display: contents">
    <button type="button" class="secondary-button apple-command-button" :disabled="state.running" :title="buttonTitle" @click="runHealthCheck">
      <LoaderCircle v-if="state.running" :size="14" class="animate-spin" /><ShieldCheck v-else :size="14" />
      {{ state.running ? (state.total ? `正在体检 ${state.done}/${state.total}` : '正在体检') : buttonLabel }}
    </button>
    <Teleport to="body">
      <div v-if="dialogOpen" class="mailbox-dialog-backdrop" role="presentation" @click.self="closeHealthCheck">
        <section data-health-panel class="panel mailbox-operation-dialog mailbox-health-dialog" role="dialog" aria-modal="true" aria-labelledby="health-check-title">
          <header class="mailbox-dialog-heading">
            <div>
              <h2 id="health-check-title"><ShieldCheck :size="18" />体检{{ title }}的邮箱能否读取</h2>
              <p>只登录读一次，不发信、不取正文。结论会写回账号列表和邮箱池。</p>
            </div>
            <button type="button" class="icon-button" title="关闭" @click="closeHealthCheck"><X :size="16" /></button>
          </header>
          <div class="mailbox-health-summary">
            <template v-if="state.running">
              <span class="font-medium">正在体检 {{ state.done }}/{{ state.total || '…' }}</span>
              <span class="text-slate-500 dark:text-slate-400">判定为凭据失效的会被标成失败并移出邮箱池</span>
            </template>
            <template v-else-if="state.report">
              <span class="font-medium">体检完成</span>
              <span class="text-slate-500 dark:text-slate-400" title="账号页是分页的，本次只覆盖当前页列出的账号。翻页后再点一次可以体检下一页">本次覆盖本页 {{ scope.accounts }} 个账号、共 {{ scope.mailboxes }} 个地址</span>
              <span class="text-emerald-600 dark:text-emerald-400">能读 {{ state.report.readable }}</span>
              <span :class="state.report.unreadable ? 'text-rose-600 dark:text-rose-400' : 'text-slate-500 dark:text-slate-400'">读不了 {{ state.report.unreadable }}</span>
              <span v-if="state.report.never_checked" class="text-amber-600 dark:text-amber-400" title="从来没有被检查过的邮箱，收信数为 0 只代表不知道">从未检查过 {{ state.report.never_checked }}</span>
              <span v-if="state.report.missing" class="text-slate-500 dark:text-slate-400">本地已无记录 {{ state.report.missing }}</span>
              <span v-if="state.report.not_covered" class="text-slate-500 dark:text-slate-400" title="这些地址的读信通道不在本次检查范围内，够不到不等于读不了">本次未覆盖 {{ state.report.not_covered }}</span>
              <span v-if="state.report.demoted" class="font-medium text-rose-600 dark:text-rose-400">已摘出邮箱池 {{ state.report.demoted }} 个</span>
              <span v-if="state.report.unreadable > state.report.demoted" class="text-amber-600 dark:text-amber-400" title="这些读不了但没摘：网络类失败不足以判定凭据失效，或者它本来就不在池里">未摘 {{ state.report.unreadable - state.report.demoted }} 个</span>
            </template>
          </div>
          <ul v-if="!state.running && unreadable.length" class="mailbox-health-list">
            <li v-for="item in unreadable" :key="item.id">
              <span class="font-mono">{{ item.email }}</span>
              <span class="text-slate-500 dark:text-slate-400">{{ item.reason || item.code }}</span>
              <span v-if="item.level === 'account'" class="rounded bg-sky-100 px-1.5 py-0.5 text-sky-700 dark:bg-sky-950/50 dark:text-sky-300">母号共用通道</span>
              <span v-if="item.demoted" class="rounded bg-rose-100 px-1.5 py-0.5 font-medium text-rose-700 dark:bg-rose-950/50 dark:text-rose-300">已摘出邮箱池</span>
              <span v-else-if="item.account_status" class="rounded bg-amber-100 px-1.5 py-0.5 font-medium text-amber-700 dark:bg-amber-950/50 dark:text-amber-300">账号已标为{{ accountStatusLabel(item) }}，未摘出邮箱池</span>
              <span v-else-if="item.offered_while_unreadable" class="rounded bg-amber-100 px-1.5 py-0.5 font-medium text-amber-700 dark:bg-amber-950/50 dark:text-amber-300">没能摘掉</span>
              <span v-else class="rounded bg-slate-200 px-1.5 py-0.5 text-slate-600 dark:bg-slate-700 dark:text-slate-300">状态未改动（{{ item.status }}）</span>
              <span v-if="item.never_checked" class="rounded bg-amber-100 px-1.5 py-0.5 text-amber-700 dark:bg-amber-950/50 dark:text-amber-300">从未检查过</span>
            </li>
          </ul>
          <p v-else-if="!state.running && state.report" class="mailbox-health-empty">这一页的邮箱都能读到信。</p>
          <footer class="mailbox-dialog-actions">
            <button type="button" class="secondary-button" :disabled="state.running" @click="closeHealthCheck">关闭</button>
          </footer>
        </section>
      </div>
    </Teleport>
  </div>
</template>
