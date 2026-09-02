import { defineConfig } from 'vitepress'

// 包文档侧边栏：分组与链接前缀参数化（zh: /packages/…、en: /en/packages/…）
function pkgGroups(prefix) {
  const P = (pkg) => `${prefix}packages/${pkg}/`
  const group = (text, pkgs, collapsed = false) => ({
    text,
    collapsed,
    items: pkgs.map((p) => ({ text: p, link: P(p) })),
  })
  return [
    { text: '总览', link: `${prefix}packages/` },
    group('内核', ['kernel', 'kernel/flow', 'kernel/flow/yaml']),
    group('模型层', ['llm', 'llm/openai', 'llm/anthropic']),
    group('执行', ['loop']),
    group('工具与技能', ['toolset', 'toolset/builtins', 'toolset/mcp', 'toolset/lsp', 'skills']),
    group('记忆', [
      'memory', 'memory/session', 'memory/compaction', 'memory/store', 'memory/assemble',
      'memory/selfedit', 'memory/index', 'memory/index/openai', 'memory/reflection', 'memory/candidate',
    ], true),
    group('可观测', ['observability']),
    group('文本处理', ['textsplit']),
    group('评测', ['eval', 'eval/war']),
    group('示例', ['examples']),
  ]
}

const zh = {
  label: '简体中文',
  lang: 'zh-CN',
  themeConfig: {
    nav: [
      { text: '指南', link: '/guide/quickstart', activeMatch: '^/guide/' },
      { text: '包文档', link: '/packages/', activeMatch: '^/packages/' },
      { text: '评测', link: '/eval', activeMatch: '^/eval' },
      { text: '示例', link: '/examples', activeMatch: '^/examples' },
    ],
    sidebar: {
      '/guide/': [
        { text: '指南', items: [
          { text: '快速开始', link: '/guide/quickstart' },
          { text: '核心概念', link: '/guide/concepts' },
          { text: 'flow 编排', link: '/guide/flow' },
          { text: '记忆层', link: '/guide/memory' },
          { text: '可观测性', link: '/guide/observability' },
        ] },
      ],
      '/packages/': pkgGroups('/'),
      '/eval': [{ text: '评测', items: [{ text: '性能基准', link: '/eval' }] }],
      '/examples': [{ text: '示例', items: [{ text: '8 课渐进示例', link: '/examples' }] }],
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
  themeConfig: {
    nav: [
      { text: 'Guide', link: '/en/guide/quickstart', activeMatch: '^/en/guide/' },
      { text: 'Packages', link: '/en/packages/', activeMatch: '^/en/packages/' },
      { text: 'Benchmarks', link: '/en/eval', activeMatch: '^/en/eval' },
      { text: 'Examples', link: '/en/examples', activeMatch: '^/en/examples' },
    ],
    sidebar: {
      '/en/guide/': [
        { text: 'Guide', items: [
          { text: 'Quick start', link: '/en/guide/quickstart' },
          { text: 'Core concepts', link: '/en/guide/concepts' },
          { text: 'flow orchestration', link: '/en/guide/flow' },
          { text: 'Memory layer', link: '/en/guide/memory' },
          { text: 'Observability', link: '/en/guide/observability' },
        ] },
      ],
      '/en/packages/': pkgGroups('/en/'),
      '/en/eval': [{ text: 'Benchmarks', items: [{ text: 'Performance', link: '/en/eval' }] }],
      '/en/examples': [{ text: 'Examples', items: [{ text: 'Lessons 00–07', link: '/en/examples' }] }],
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
  description: 'Go AI Agent 框架——可逆效应与依赖响应式的插件内核，v2 核心已以 v0.1.0 预览发布',
  head: [['link', { rel: 'icon', type: 'image/svg+xml', href: '/favicon.svg' }]],
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
      message: '开源 · MIT · v0.1.0 预览',
      copyright: 'Copyright © 2026 Luo-root',
    },
  },
})
