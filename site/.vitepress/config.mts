import { defineConfig } from 'vitepress'

function sidebar(prefix: string, en: boolean) {
  const link = (path: string) => `${prefix}${path}`
  const labels = en
    ? ['Getting started', 'Usage guides', 'Reference', 'Packages']
    : ['入门', '使用指南', '参考', '包文档']
  const pages = (items: string[][]) => items.map(([zh, english, path]) => ({
    text: en ? english : zh, link: link(path),
  }))
  return [
    { text: labels[0], items: pages([
      ['介绍', 'Introduction', 'guide/'],
      ['安装与版本', 'Installation and versions', 'guide/installation'],
      ['快速开始', 'Quick start', 'guide/quickstart'],
      ['核心概念', 'Core concepts', 'guide/concepts'],
    ]) },
    { text: labels[1], items: pages([
      ['指南总览', 'Overview', 'guide/orchestration'],
      ['输入与输出', 'Inputs and outputs', 'guide/inputs-outputs'],
      ['分支与汇聚', 'Branches and joins', 'guide/branching'],
      ['重试与取消', 'Retries and cancellation', 'guide/retry-cancellation'],
      ['组合子图', 'Subgraphs', 'guide/subgraphs'],
      ['业务组合', 'Business composition', 'guide/composition'],
      ['流式处理', 'Streams', 'guide/streams'],
      ['YAML 装配', 'YAML assembly', 'guide/assembly'],
      ['图观测', 'Observation', 'guide/observability'],
    ]) },
    { text: labels[2], collapsed: false, items: pages([
      ['参考索引', 'Reference index', 'reference/'],
      ['执行生命周期', 'Execution lifecycle', 'reference/lifecycle'],
      ['错误与排查', 'Errors and troubleshooting', 'reference/errors'],
      ['检查点桥', 'Checkpoint bridge', 'reference/checkpoint'],
      ['版本与迁移', 'Versions and migration', 'reference/migration'],
      ['统一生产性能对比', 'Unified production comparison', 'reference/production-comparison'],
      ['设计理念', 'Design philosophy', 'guide/philosophy'],
    ]) },
    { text: labels[3], collapsed: true, items: pages([
      ['包总览', 'Package overview', 'packages/'],
      ['observe', 'observe', 'packages/observe/'],
      ['yaml', 'yaml', 'packages/yaml/'],
    ]) },
  ]
}

function locale(en: boolean) {
  const prefix = en ? '/en/' : '/'
  const groups = sidebar(prefix, en)
  return {
    label: en ? 'English' : '简体中文',
    lang: en ? 'en-US' : 'zh-CN',
    ...(en ? { link: '/en/' } : {}),
    description: en ? 'Data-driven workflow execution for Go.' : 'Go 的数据驱动工作流执行库。',
    themeConfig: {
      nav: [
        { text: en ? 'Get started' : '入门', link: `${prefix}guide/`, activeMatch: `${prefix}guide/(?:installation|quickstart|concepts)?(?:\\.html)?$` },
        { text: en ? 'Guides' : '使用指南', link: `${prefix}guide/orchestration`, activeMatch: `${prefix}guide/(?:orchestration|inputs-outputs|branching|retry-cancellation|subgraphs|composition|streams|assembly|observability)` },
        { text: en ? 'Reference' : '参考', link: `${prefix}reference/`, activeMatch: `${prefix}(?:reference/|packages/|guide/philosophy)` },
        { text: 'Durable', link: 'https://github.com/Luo-root/pulse-durable' },
      ],
      sidebar: {
        [`${prefix}guide/`]: groups,
        [`${prefix}reference/`]: groups,
        [`${prefix}packages/`]: groups,
      },
      outline: { level: [2, 3] as [number, number], label: en ? 'On this page' : '本页目录' },
      docFooter: { prev: en ? 'Previous' : '上一篇', next: en ? 'Next' : '下一篇' },
      editLink: {
        pattern: ({ filePath }: { filePath: string }) => {
          const generated = filePath.match(/^(en\/)?packages\/(observe|yaml)\/index\.md$/)
          const source = generated
            ? `${generated[2]}/${generated[1] ? 'README_en.md' : 'README.md'}`
            : `site/${filePath}`
          return `https://github.com/Luo-root/pulse/edit/main/${source}`
        },
        text: en ? 'Edit this page on GitHub' : '在 GitHub 编辑此页',
      },
      footer: {
        message: en ? 'Development API documentation · Go 1.26+ · MIT' : '开发版 API 文档 · Go 1.26+ · MIT',
        copyright: 'Copyright © 2026 Luo-root',
      },
      ...(en ? {} : {
        darkModeSwitchLabel: '外观', lightModeSwitchTitle: '切换到浅色主题',
        darkModeSwitchTitle: '切换到深色主题', sidebarMenuLabel: '目录',
        returnToTopLabel: '返回顶部', langMenuLabel: '切换语言',
      }),
    },
  }
}

export default defineConfig({
  base: '/pulse/',
  lang: 'zh-CN',
  title: 'Pulse',
  description: 'Go 的数据驱动工作流执行库。',
  head: [['link', { rel: 'icon', type: 'image/svg+xml', href: '/pulse/favicon.svg' }]],
  locales: { root: locale(false), en: locale(true) },
  themeConfig: {
    logo: '/logo.svg',
    siteTitle: 'Pulse',
    socialLinks: [{ icon: 'github', link: 'https://github.com/Luo-root/pulse' }],
    search: {
      provider: 'local',
      options: {
        locales: {
          root: {
            translations: {
              button: { buttonText: '搜索文档', buttonAriaLabel: '搜索文档' },
              modal: { noResultsText: '没有结果', resetButtonTitle: '清除查询',
                footer: { selectText: '选择', navigateText: '切换', closeText: '关闭' } },
            },
          },
        },
      },
    },
  },
})
