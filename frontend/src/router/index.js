import { createRouter, createWebHistory } from 'vue-router'
import { loadAuthStatus } from '../composables/useAuth'
import AppLayout from '../layouts/AppLayout.vue'
import LoginView from '../views/LoginView.vue'
import DashboardView from '../views/DashboardView.vue'
import AppleAccountsView from '../views/AppleAccountsView.vue'
import MicrosoftAccountsView from '../views/MicrosoftAccountsView.vue'
import GoogleAccountsView from '../views/GoogleAccountsView.vue'
import ImapAccountsView from '../views/ImapAccountsView.vue'
import MailboxesView from '../views/MailboxesView.vue'
import TasksView from '../views/TasksView.vue'
import SettingsView from '../views/SettingsView.vue'
import ExportsView from '../views/ExportsView.vue'
import NotFoundView from '../views/NotFoundView.vue'
import PublicCodeView from '../views/PublicCodeView.vue'

const router = createRouter({
  history: createWebHistory(),
  scrollBehavior(to, _from, savedPosition) {
    if (savedPosition) return savedPosition
    if (to.hash) return { el: to.hash, top: 80, behavior: 'smooth' }
    return { top: 0 }
  },
  routes: [
    { path: '/login', name: 'login', component: LoginView, meta: { public: true, title: '登录' } },
    { path: '/email-code', name: 'email-code', component: PublicCodeView, meta: { public: true, title: '邮箱取码' } },
    {
      path: '/',
      component: AppLayout,
      children: [
        { path: '', name: 'dashboard', component: DashboardView, meta: { title: '控制台', subtitle: '查看账号、邮箱和运行状态' } },
        { path: 'apple-accounts', name: 'apple-accounts', component: AppleAccountsView, meta: { title: 'Apple 账号', subtitle: '管理登录态、IMAP、创建与远端同步' } },
        { path: 'microsoft-accounts', name: 'microsoft-accounts', component: MicrosoftAccountsView, meta: { title: 'Microsoft 账号', subtitle: '通过 Microsoft Graph 读取 Outlook 别名验证码' } },
        { path: 'google-accounts', name: 'google-accounts', component: GoogleAccountsView, meta: { title: 'Google 账号', subtitle: '通过 Gmail API 读取 + 标签地址验证码' } },
        // QQ 和 163 共用同一个组件，靠 meta.preset 分开。
        // 两者协议完全相同，只是服务器地址不同，复制一份页面没有意义。
        { path: 'qq-accounts', name: 'qq-accounts', component: ImapAccountsView, meta: { title: 'QQ 邮箱', subtitle: '母号用 IMAP + 授权码收信，名下地址默认不进邮箱池', preset: 'qq', presetName: 'QQ 邮箱', presetLabel: 'QQ邮箱' } },
        { path: 'mail-163-accounts', name: 'mail-163-accounts', component: ImapAccountsView, meta: { title: '163 邮箱', subtitle: '母号用 IMAP + 授权码收信，名下地址默认不进邮箱池', preset: '163', presetName: '163 邮箱', presetLabel: '163邮箱' } },
        { path: 'mailboxes', name: 'mailboxes', component: MailboxesView, meta: { title: '邮箱池', subtitle: '收信、取码、状态维护与 Apple 远端删除' } },
        { path: 'tasks', name: 'tasks', component: TasksView, meta: { title: '创建隐私邮箱', subtitle: '创建一个邮箱或配置自动创建' } },
        { path: 'exports', name: 'exports', component: ExportsView, meta: { title: '本地导出', subtitle: '导出运行数据、邮件、邮箱地址和取码 API' } },
        { path: 'settings', name: 'settings', component: SettingsView, meta: { title: '系统设置', subtitle: '管理单用户本地运行参数' } },
      ],
    },
    { path: '/:pathMatch(.*)*', name: 'not-found', component: NotFoundView, meta: { public: true, title: '页面不存在' } },
  ],
})

router.beforeEach(async (to) => {
  document.title = `${to.meta.title || 'iCloud Privacy Mail'} · iCloud Privacy Mail`
  const state = await loadAuthStatus()
  if (to.meta.public) {
    if (to.name === 'login' && state.authenticated) return { name: 'dashboard' }
    return true
  }
  if (!state.authenticated) return { name: 'login', query: { redirect: to.fullPath } }
  return true
})

export default router
