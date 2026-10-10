import { defineConfig } from 'vitepress'

// 版本号单源：footer 引用它——发版时只改这一处（站点文档描述的是 main 上的 API，
// 不要在这里把「文档内容 = 某个 tag 的内容」写成声明）。
const VERSION = 'v0.4.0'

// 包文档侧边栏：pulse 只有三个包——根包 pulse（引擎，文档在指南里）、
// observe（图观测）、yaml（声明式装图）。后两者有包 README，由 sync-docs.mjs 同步成页面。
const ZH_LABELS = { overview: '总览', obs: '图观测', asm: '声明式装图' }
const EN_LABELS = { overview: 'Overview', obs: 'Observation', asm: 'YAML assembly' }

function pkgGroups(prefix, L = ZH_LABELS) {
  const P = (pkg) => `${prefix}packages/${pkg}/`
  return [
    { text: L.overview, link: `${prefix}packages/` },
    { text: L.obs, items: [{ text: 'observe', link: P('observe') }] },
    { text: L.asm, items: [{ text: 'yaml', link: P('yaml') }] },
  ]
}

const zh = {
  label: '简体中文',
  lang: 'zh-CN',
  themeConfig: {
    nav: [
      { text: '指南', link: '/guide/quickstart', activeMatch: '^/guide/' },
      { text: '包文档', link: '/packages/', activeMatch: '^/packages/' },
    ],
    sidebar: {
      '/guide/': [
        { text: '指南', items: [
          { text: '快速开始', link: '/guide/quickstart' },
          { text: '核心概念', link: '/guide/concepts' },
          { text: '编排', link: '/guide/orchestration' },
          { text: '声明式装图', link: '/guide/assembly' },
          { text: '图观测', link: '/guide/observability' },
        ] },
      ],
      '/packages/': pkgGroups('/'),
    },
    search: {
      provider: 'local',
      options: {
        translations: {
          button: { buttonText: '搜索文档', buttonAriaLabel: '搜索文档' },
          modal: {
            noResultsText: '没有结果',
            resetButtonTitle: '清除查询',
            footer: { selectText: '选择', navigateText: '切换', closeText: '关闭' },
          },
        },
      },
    },
    outline: { label: '本页目录' },
    docFooter: { prev: '上一篇', next: '下一篇' },
    darkModeSwitchLabel: '外观',
    lightModeSwitchTitle: '切换到浅色主题',
    darkModeSwitchTitle: '切换到深色主题',
    sidebarMenuLabel: '菜单',
    returnToTopLabel: '返回顶部',
    langMenuLabel: '切换语言',
  },
}

const en = {
  label: 'English',
  lang: 'en-US',
  link: '/en/',
  description: `A one-shot graph engine for Go — data-arrival scheduling, explicit failure, graph observation.`,
  themeConfig: {
    nav: [
      { text: 'Guide', link: '/en/guide/quickstart', activeMatch: '^/en/guide/' },
      { text: 'Packages', link: '/en/packages/', activeMatch: '^/en/packages/' },
    ],
    sidebar: {
      '/en/guide/': [
        { text: 'Guide', items: [
          { text: 'Quick start', link: '/en/guide/quickstart' },
          { text: 'Core concepts', link: '/en/guide/concepts' },
          { text: 'Orchestration', link: '/en/guide/orchestration' },
          { text: 'Declarative assembly', link: '/en/guide/assembly' },
          { text: 'Graph observation', link: '/en/guide/observability' },
        ] },
      ],
      '/en/packages/': pkgGroups('/en/', EN_LABELS),
    },
    footer: {
      message: `Open Source · MIT · ${VERSION}`,
      copyright: 'Copyright © 2026 Luo-root',
    },
    search: { provider: 'local' },
    outline: { label: 'On this page' },
    docFooter: { prev: 'Previous', next: 'Next' },
  },
}

export default defineConfig({
  base: '/pulse/',
  lang: 'zh-CN',
  title: 'Pulse',
  description: `Go 的一次性图引擎——数据到达即调度，失败显式；外加一层图观测。`,
  head: [['link', { rel: 'icon', type: 'image/svg+xml', href: '/pulse/favicon.svg' }]], // head 里的自定义 link 不吃 base 自动前缀，硬编码（与 base 同步）
  locales: { root: zh, en },
  // v1.x dead-link checker 会把「目录尾斜杠链接」(/dir/) 规范化为 /dir/index 后查路由表，
  // 而路由表条目是目录形式 → 纯误报（GH Pages 与 SPA 运行时均正确解析尾斜杠目录链接）。
  // 链接正确性由 sync-docs.mjs 的确定性改写 + 构建后 dist 抽查保证。
  ignoreDeadLinks: true,
  themeConfig: {
    logo: '/logo.svg',
    siteTitle: 'Pulse',
    socialLinks: [{ icon: 'github', link: 'https://github.com/Luo-root/pulse' }],
    footer: {
      message: `开源 · MIT · ${VERSION}`,
      copyright: 'Copyright © 2026 Luo-root',
    },
  },
})
