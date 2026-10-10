<script setup>
import { computed } from 'vue'
import { useData } from 'vitepress'

// 版本号单源：badge / tagline / CTA / Release 链接都引用它——发版只改这一处。
const VERSION = 'v0.4.1'
const RELEASE_URL = `https://github.com/Luo-root/pulse/releases/tag/${VERSION}`
const DESIGN_URL = 'https://github.com/Luo-root/pulse/blob/main/docs/design/pulse.md'

const { lang } = useData()
const isZh = computed(() => lang.value.startsWith('zh'))

const copy = {
  zh: {
    badge: `${VERSION} · 开源 MIT`,
    heroTitle: '数据到达即调度，',
    heroTitleAccent: '失败显式',
    tagline: `Pulse 是一个一次性运行的 Go 图引擎。节点只声明读哪些 Key、写哪些 Key，拓扑由数据的生产与消费隐式形成——没有边对象、没有拓扑排序、没有调度循环。引擎的依赖闭包为零；图观测是独立的一层，只经一个 Observer seam 接进来。`,
    ctaStart: '快速开始',
    ctaPackages: '包文档',
    ctaGithub: 'GitHub',
    featEyebrow: '核心能力',
    featTitle: '编排与观测，两件事各归其位',
    feats: [
      { icon: 'flow', title: '一次性图引擎', desc: '依赖声明即拓扑，数据到达即调度。Graph 是模板的一次实例，不是可重跑的容器——复用模板就再 New 一次。引擎只用标准库。', link: '/pulse/guide/concepts', linkText: '核心概念' },
      { icon: 'kernel', title: '槽位三态', desc: 'pending | ready(值) | skipped。就绪与跳过都是「到达」——跳过不是失败，分支就靠对未选中的 Provide 调用 Skip 写出。', link: '/pulse/guide/concepts', linkText: '看槽位契约' },
      { icon: 'loop', title: '切面与取消', desc: 'Aspect 包住「等输入 + 执行」整段，所以 Timeout 能打断还在等数据的节点；Retry 只对执行错误重试，跳过与取消都不重试。', link: '/pulse/guide/orchestration', linkText: '编排指南' },
      { icon: 'eval', title: '声明式装图', desc: 'YAML 拥有拓扑，注册的工厂只给 Run；Seed 的取值交给宿主——引擎不做 IO。', link: '/pulse/guide/assembly', linkText: '装配指南' },
      { icon: 'obs', title: '图观测', desc: '引擎每节点至多三条回调；折叠成结构化记录是 observe 的事。业务维度一律走 Attrs，不扩具名字段。', link: '/pulse/guide/observability', linkText: '观测指南' },
      { icon: 'llm', title: '宿主自带出口', desc: '换掉行体渲染器，用六条导出的编码原语拼自己的列——与内置版式逐字节同形，出口仍然不认识任何业务语义。', link: '/pulse/guide/observability', linkText: '看渲染器契约' },
    ],
    startEyebrow: '开始使用',
    startTitle: '两分钟起一张图',
    start1Title: '安装',
    start1Desc: '需要 Go 1.25+。只有根包时依赖闭包为零——不需要观测的宿主只 import 它。',
    start2Title: '起一张图',
    start2Desc: '声明读写的 Key，装节点，Run。本轮的数据随 Run 而生、随结束而灭；跨运行的状态归调用方。',
    ctaTitle: '从一个最小的图引擎开始',
    ctaDesc: '设计文档只写两件事：编排与观测。API 契约写在 godoc 里——每个导出符号都带一条。',
    ctaDesign: '设计文档',
    ctaRelease: `${VERSION} Release`,
  },
  en: {
    badge: `${VERSION} · Open Source MIT`,
    heroTitle: 'Data arrival is scheduling, ',
    heroTitleAccent: 'failure is explicit',
    tagline: `Pulse is a one-shot graph engine for Go. A node declares only which keys it reads and which it writes; topology is implied by data production and consumption — no edge objects, no topological sort, no scheduler loop. The engine's dependency closure is empty, and graph observation is a separate layer that attaches through a single Observer seam.`,
    ctaStart: 'Quick start',
    ctaPackages: 'Package docs',
    ctaGithub: 'GitHub',
    featEyebrow: 'Core capabilities',
    featTitle: 'Orchestration and observation, each in its own place',
    feats: [
      { icon: 'flow', title: 'One-shot graph engine', desc: 'Dependency declarations are the topology; data arrival is the schedule. A Graph is one instantiation of a template, not a re-runnable container — reuse the template by calling New again. Standard library only.', link: '/pulse/en/guide/concepts', linkText: 'Core concepts' },
      { icon: 'kernel', title: 'Three slot states', desc: 'pending | ready(value) | skipped. Ready and skipped are both arrival — a skip is not a failure. Branching is written by calling Skip on the Provide you did not choose.', link: '/pulse/en/guide/concepts', linkText: 'Slot contract' },
      { icon: 'loop', title: 'Aspects and cancellation', desc: 'An Aspect wraps the whole "wait for input + execute" span, so Timeout can interrupt a node still waiting for data; Retry retries execution errors only — never a skip, never a cancellation.', link: '/pulse/en/guide/orchestration', linkText: 'Orchestration guide' },
      { icon: 'eval', title: 'Declarative assembly', desc: 'The YAML owns the topology and a registered factory supplies only Run; seed resolution is the host\'s job — the engine does no IO.', link: '/pulse/en/guide/assembly', linkText: 'Assembly guide' },
      { icon: 'obs', title: 'Graph observation', desc: 'At most three callbacks per node; folding them into structured records is observe\'s job. Business dimensions ride Attrs only — never named fields.', link: '/pulse/en/guide/observability', linkText: 'Observation guide' },
      { icon: 'llm', title: 'Host-supplied egress', desc: 'Swap the line renderer and build your own columns from six exported encoding primitives — byte-for-byte identical to the built-in layout, and the egress still knows nothing about your domain.', link: '/pulse/en/guide/observability', linkText: 'Renderer contract' },
    ],
    startEyebrow: 'Get started',
    startTitle: 'A graph in two minutes',
    start1Title: 'Install',
    start1Desc: 'Requires Go 1.25+. With only the root package the dependency closure is empty — a host that needs no observation imports just that.',
    start2Title: 'Build a graph',
    start2Desc: 'Declare the keys, add nodes, Run. This run\'s data lives and dies with Run; cross-run state belongs to the caller.',
    ctaTitle: 'Start from a minimal graph engine',
    ctaDesc: 'The design doc covers exactly two things: orchestration and observation. API contracts live in godoc — every exported symbol carries one.',
    ctaDesign: 'Design doc',
    ctaRelease: `${VERSION} Release`,
  },
}

const t = computed(() => (isZh.value ? copy.zh : copy.en))

const icons = {
  kernel: 'M4 4h16v16H4z M12 8.5a3.5 3.5 0 1 0 0 7 3.5 3.5 0 0 0 0-7z',
  llm: 'M4 6a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H10l-4.5 4V17H6a2 2 0 0 1-2-2V6z M9 9.5h6 M9 12.5h4',
  loop: 'M20 12a8 8 0 1 1-2.4-5.7 M20 4v4.5h-4.5',
  flow: 'M3 12h5c3 0 3-6 6.5-6H21 M8 12c3 0 3 6 6.5 6H21',
  obs: 'M2.5 12S6 5.8 12 5.8 21.5 12 21.5 12 18 18.2 12 18.2 2.5 12 2.5 12z M12 14.6a2.6 2.6 0 1 0 0-5.2 2.6 2.6 0 0 0 0 5.2z',
  eval: 'M5 20v-7 M12 20V5.5 M19 20V9.5 M3 20h18',
}
</script>

<template>
  <div class="home">
    <!-- ===== Hero：固定深色 + 光晕 + 品牌波形装饰 ===== -->
    <section class="hero">
      <div class="hero-bg" aria-hidden="true">
        <div class="glow glow-a"></div>
        <div class="glow glow-b"></div>
        <div class="grid"></div>
        <svg class="pulse-mark" viewBox="0 0 48 48" fill="none">
          <path d="M5 27h10.5l5-13.5 6.5 27 5-13.5H43" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" />
          <circle cx="20.5" cy="13.5" r="2.6" fill="currentColor" />
        </svg>
      </div>
      <div class="shell hero-shell">
        <span class="badge">{{ t.badge }}</span>
        <h1 class="hero-title">{{ t.heroTitle }}<span class="accent">{{ t.heroTitleAccent }}</span></h1>
        <p class="tagline">{{ t.tagline }}</p>
        <div class="actions">
          <a class="btn btn-primary" href="/pulse/guide/quickstart">{{ t.ctaStart }}</a>
          <a class="btn btn-ghost" href="/pulse/packages/">{{ t.ctaPackages }}</a>
          <a class="btn btn-ghost" href="https://github.com/Luo-root/pulse" target="_blank" rel="noopener">
            <svg width="15" height="15" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z" /></svg>
            {{ t.ctaGithub }}
          </a>
        </div>
      </div>
    </section>

    <!-- ===== 核心能力 ===== -->
    <section class="block">
      <div class="shell">
        <span class="eyebrow">{{ t.featEyebrow }}</span>
        <h2 class="h2">{{ t.featTitle }}</h2>
        <div class="cards">
          <a v-for="f in t.feats" :key="f.title" class="card" :href="f.link">
            <span class="card-icon" aria-hidden="true">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
                <path :d="icons[f.icon]" />
              </svg>
            </span>
            <span class="card-title">{{ f.title }}</span>
            <span class="card-desc">{{ f.desc }}</span>
            <span class="card-link">{{ f.linkText }} →</span>
          </a>
        </div>
      </div>
    </section>

    <!-- ===== 开始使用 ===== -->
    <section class="block block-soft">
      <div class="shell">
        <span class="eyebrow">{{ t.startEyebrow }}</span>
        <h2 class="h2">{{ t.startTitle }}</h2>
        <div class="cards two">
          <div class="card start-card">
            <span class="card-title">{{ t.start1Title }}</span>
            <span class="card-desc">{{ t.start1Desc }}</span>
            <code class="code"><span class="dollar">$ </span>go get github.com/Luo-root/pulse</code>
          </div>
          <div class="card start-card">
            <span class="card-title">{{ t.start2Title }}</span>
            <span class="card-desc">{{ t.start2Desc }}</span>
            <code class="code">g, _ := pulse.New(ctx, "demo")</code>
          </div>
        </div>
      </div>
    </section>

    <!-- ===== CTA ===== -->
    <section class="block cta">
      <div class="shell cta-shell">
        <h2 class="h2">{{ t.ctaTitle }}</h2>
        <p class="cta-desc">{{ t.ctaDesc }}</p>
        <div class="actions center">
          <a class="btn btn-primary" href="https://github.com/Luo-root/pulse" target="_blank" rel="noopener">{{ t.ctaGithub }}</a>
          <a class="btn btn-ghost" :href="DESIGN_URL" target="_blank" rel="noopener">{{ t.ctaDesign }}</a>
          <a class="btn btn-ghost" :href="RELEASE_URL" target="_blank" rel="noopener">{{ t.ctaRelease }}</a>
        </div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.home {
  margin-inline: calc(50% - 50vw); /* 撑出 .vp-doc 容器，铺满视口 */
}

.shell {
  max-width: 1152px;
  margin-inline: auto;
  padding-inline: 24px;
}

/* ---- Hero ---- */
.hero {
  position: relative;
  overflow: hidden;
  color: #e6ebf5;
  background: radial-gradient(120% 140% at 50% 0%, #101b3a 0%, #0a1024 55%, #070b18 100%);
  padding: 104px 0 112px;
}
.hero-bg {
  position: absolute;
  inset: 0;
  pointer-events: none;
}
.glow {
  position: absolute;
  border-radius: 50%;
  filter: blur(90px);
}
.glow-a {
  width: 640px;
  height: 420px;
  left: 8%;
  bottom: -30%;
  background: radial-gradient(circle, rgba(30, 64, 175, 0.55) 0%, transparent 70%);
}
.glow-b {
  width: 520px;
  height: 360px;
  right: 4%;
  top: -20%;
  background: radial-gradient(circle, rgba(37, 99, 235, 0.4) 0%, transparent 70%);
}
.grid {
  position: absolute;
  inset: 0;
  background-image: linear-gradient(rgba(148, 163, 184, 0.05) 1px, transparent 1px),
    linear-gradient(90deg, rgba(148, 163, 184, 0.05) 1px, transparent 1px);
  background-size: 56px 56px;
  mask-image: radial-gradient(75% 90% at 50% 10%, black 30%, transparent 100%);
  -webkit-mask-image: radial-gradient(75% 90% at 50% 10%, black 30%, transparent 100%);
}
.pulse-mark {
  position: absolute;
  width: 560px;
  right: -60px;
  top: 50%;
  transform: translateY(-50%);
  color: #3b82f6;
  opacity: 0.14;
}
.hero-shell {
  position: relative;
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 20px;
}
.badge {
  display: inline-block;
  border: 1px solid rgba(96, 165, 250, 0.4);
  border-radius: 999px;
  background: rgba(37, 99, 235, 0.14);
  color: #93c5fd;
  font-size: 13px;
  font-weight: 500;
  letter-spacing: 0.02em;
  padding: 4px 12px;
}
.hero-title {
  font-size: 52px;
  line-height: 1.15;
  font-weight: 800;
  letter-spacing: -0.01em;
  color: #f4f7fd;
  margin: 0;
  max-width: 720px;
}
.accent {
  background: linear-gradient(100deg, #60a5fa, #38bdf8);
  -webkit-background-clip: text;
  background-clip: text;
  color: transparent;
}
.tagline {
  font-size: 17px;
  line-height: 1.7;
  color: #9fb0cc;
  max-width: 560px;
  margin: 0;
}
.actions {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 8px;
}
.actions.center {
  justify-content: center;
}
.btn {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  border-radius: 10px;
  padding: 10px 20px;
  font-size: 15px;
  font-weight: 600;
  transition: transform 0.15s ease, box-shadow 0.15s ease, background 0.15s ease, border-color 0.15s ease;
}
.btn:hover {
  text-decoration: none;
  transform: translateY(-1px);
}
.btn-primary {
  background: linear-gradient(135deg, #2563eb, #1d4ed8);
  color: #fff;
  box-shadow: 0 8px 24px rgba(37, 99, 235, 0.35);
}
.btn-primary:hover {
  color: #fff;
  box-shadow: 0 10px 28px rgba(37, 99, 235, 0.5);
}
.btn-ghost {
  border: 1px solid rgba(148, 163, 184, 0.35);
  color: #c8d3e8;
}
.btn-ghost:hover {
  border-color: rgba(96, 165, 250, 0.6);
  color: #e6ebf5;
  background: rgba(37, 99, 235, 0.1);
}

/* ---- Blocks ---- */
.block {
  padding: 84px 0;
}
.block-soft {
  background: var(--vp-c-bg-soft);
}
.eyebrow {
  display: inline-block;
  font-size: 13px;
  font-weight: 600;
  letter-spacing: 0.12em;
  text-transform: uppercase;
  color: var(--vp-c-brand-1);
  margin-bottom: 10px;
}
.h2 {
  font-size: 34px;
  font-weight: 800;
  letter-spacing: -0.01em;
  margin: 0 0 40px;
  max-width: 720px;
}
.cards {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 18px;
}
.cards.two {
  grid-template-columns: repeat(2, 1fr);
}
.card {
  display: flex;
  flex-direction: column;
  gap: 10px;
  border: 1px solid var(--vp-c-divider);
  border-radius: 14px;
  background: var(--vp-c-bg);
  padding: 24px;
  transition: border-color 0.2s ease, transform 0.2s ease, box-shadow 0.2s ease;
}
a.card:hover {
  text-decoration: none;
  border-color: var(--vp-c-brand-1);
  transform: translateY(-3px);
  box-shadow: 0 12px 32px rgba(37, 99, 235, 0.12);
}
.card-icon {
  display: inline-flex;
  width: 40px;
  height: 40px;
  align-items: center;
  justify-content: center;
  border-radius: 10px;
  background: var(--vp-c-brand-soft);
  color: var(--vp-c-brand-1);
}
.card-icon svg {
  width: 22px;
  height: 22px;
}
.card-title {
  font-size: 17px;
  font-weight: 700;
  color: var(--vp-c-text-1);
}
.card-desc {
  font-size: 14px;
  line-height: 1.7;
  color: var(--vp-c-text-2);
}
.card-link {
  margin-top: auto;
  font-size: 13.5px;
  font-weight: 600;
  color: var(--vp-c-brand-1);
}
.code {
  display: block;
  margin-top: 10px;
  padding: 12px 16px;
  border-radius: 10px;
  background: var(--vp-c-bg-alt, #0d1117);
  border: 1px solid var(--vp-c-divider);
  color: #7dd3fc;
  font-family: var(--vp-font-family-mono, ui-monospace, monospace);
  font-size: 13.5px;
  overflow-x: auto;
}
.dollar {
  color: #475569;
  user-select: none;
}

/* ---- CTA ---- */
.cta {
  text-align: center;
}
.cta-shell {
  display: flex;
  flex-direction: column;
  align-items: center;
}
.cta-shell .h2 {
  margin-bottom: 16px;
}
.cta-desc {
  font-size: 16px;
  line-height: 1.7;
  color: var(--vp-c-text-2);
  max-width: 560px;
  margin: 0 0 28px;
}
.cta .actions {
  margin-top: 0;
}
.cta .btn-ghost {
  border-color: var(--vp-c-divider);
  color: var(--vp-c-text-1);
}
.cta .btn-ghost:hover {
  border-color: var(--vp-c-brand-1);
  color: var(--vp-c-brand-1);
}

/* ---- 响应式 ---- */
@media (max-width: 960px) {
  .cards {
    grid-template-columns: repeat(2, 1fr);
  }
  .pulse-mark {
    width: 420px;
    opacity: 0.1;
  }
  .hero-title {
    font-size: 42px;
  }
}
@media (max-width: 640px) {
  .cards,
  .cards.two {
    grid-template-columns: 1fr;
  }
  .hero {
    padding: 72px 0 80px;
  }
  .hero-title {
    font-size: 34px;
  }
  .pulse-mark {
    display: none;
  }
}
</style>
