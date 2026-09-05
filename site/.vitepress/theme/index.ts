import DefaultTheme from 'vitepress/theme'
import Home from './components/Home.vue'
import './custom.css'

// mermaid：客户端按需渲染 markdown 里的 ```mermaid 代码块。
// 仅动态 import（SSR 侧不加载，无图页面零加载）；主题固定浅色
// base + 脉冲蓝变量，与 GitHub 上同图观感一致。渲染失败的块保留
// 原代码块，不阻塞页面。
let mermaidReady = false
let seq = 0

async function renderMermaid() {
  if (typeof document === 'undefined') return
  // VitePress 1.6 把 ```mermaid fence 渲染为
  // <div class="language-mermaid vp-adaptive-theme">…<pre><code>…</code></pre></div>：
  // language-mermaid 在包装 div 上（兼容裸 code 形态），替换目标是整个包装容器。
  const boxes = document.querySelectorAll<HTMLElement>('.language-mermaid')
  if (boxes.length === 0) return
  const mermaid = (await import('mermaid')).default
  if (!mermaidReady) {
    mermaid.initialize({
      startOnLoad: false,
      theme: 'base',
      // 节点内边距：给 CJK 字体 metrics 超出行盒的部分留出余量
      flowchart: { padding: 16 },
      themeVariables: {
        fontFamily: "'Segoe UI', 'PingFang SC', 'Microsoft YaHei', sans-serif",
        fontSize: '13px',
        primaryColor: '#eff6ff',
        primaryBorderColor: '#2563eb',
        primaryTextColor: '#1e293b',
        lineColor: '#60a5fa',
        clusterBkg: '#f8fafc',
        clusterBorder: '#dbeafe',
        edgeLabelBackground: '#ffffff',
      },
    })
    mermaidReady = true
  }
  for (const box of Array.from(boxes)) {
    const isBareCode = box.tagName === 'CODE'
    const code = isBareCode ? box : box.querySelector<HTMLElement>('pre > code')
    const container = isBareCode ? box.parentElement : box
    if (!code || !container) continue
    const src = code.textContent ?? ''
    try {
      const { svg } = await mermaid.render('pulse-mmd-' + ++seq, src)
      const holder = document.createElement('div')
      holder.className = 'mermaid-block'
      holder.dataset.src = src // 保留源码，供暗色重渲染等后续增强
      holder.innerHTML = svg
      container.replaceWith(holder)
    } catch (err) {
      console.error('[pulse-site] mermaid render failed', err)
    }
  }
}

export default {
  extends: DefaultTheme,
  enhanceApp({ app, router }) {
    // 全局注册自定义首页组件：md 页面直接 <Home /> 使用
    //（md 内 import '.vitepress/…' 相对路径在 rollup 虚拟模块下不可解析，故走全局注册）
    app.component('Home', Home)
    // 路由切换后重扫 mermaid 代码块（SPA 下每次导航内容 DOM 重建）；
    // 首屏由 enhanceApp 后挂载完成时的定时兜底触发。
    if (!import.meta.env.SSR && router) {
      router.onAfterRouteChange = () => {
        void renderMermaid()
      }
      setTimeout(() => {
        void renderMermaid()
      }, 300)
    }
  },
}
